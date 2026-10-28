package postgres_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// running is a database created and provisioned, with a table holding a
// row, as a customer would leave it.
func (p *provisioning) running(name string) instances.Created {
	p.t.Helper()
	c := p.create(name)
	p.runOnce()
	if o := p.outcome(c); o.Instance != "running" {
		p.t.Fatalf("create: %+v", o)
	}
	db, err := p.asTenantVia("pool-port", postgres.Tenant{Name: c.Instance.InfraName}, c.Password)
	if err != nil {
		p.t.Fatal(err)
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(context.Background(), `CREATE TABLE notes (body text); INSERT INTO notes VALUES ('keep me')`); err != nil {
		p.t.Fatal(err)
	}
	return c
}

// delete asks for the delete through the API's path; the returned
// operation is the delete's.
func (p *provisioning) delete(c instances.Created) instances.Created {
	p.t.Helper()
	ctx := context.Background()
	tx, err := p.db.ConnAs(dbtest.RoleAPI).Begin(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, op, err := instances.Delete(ctx, tx, p.account, c.Instance.ID, c.Instance.Name, "user:"+p.user)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		p.t.Fatal(err)
	}
	c.Operation.ID = op.ID
	return c
}

func (p *provisioning) exists(kind, name string) bool {
	p.t.Helper()
	ctx := context.Background()
	conn := p.admin()
	defer conn.Close(ctx)
	var ok bool
	q := map[string]string{"role": `SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1)`,
		"database": `SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)`}[kind]
	if err := conn.QueryRow(ctx, q, name).Scan(&ok); err != nil {
		p.t.Fatal(err)
	}
	return ok
}

type dumpRecord struct {
	Ref, SHA256 string
	Size        int64
	Expires     time.Duration
}

func (p *provisioning) dumps(c instances.Created) []dumpRecord {
	p.t.Helper()
	rows, err := p.db.ConnAs(dbtest.RoleMigrator).Query(context.Background(), `
		SELECT ref, sha256, size_bytes, extract(epoch FROM expires_at - now())
		FROM backup_records WHERE instance_id = $1 AND kind = 'final_dump' AND status = 'ok' AND target = 'local'`, c.Instance.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []dumpRecord
	for rows.Next() {
		var r dumpRecord
		var secs float64
		if err := rows.Scan(&r.Ref, &r.SHA256, &r.Size, &secs); err != nil {
			p.t.Fatal(err)
		}
		r.Expires = time.Duration(secs) * time.Second
		out = append(out, r)
	}
	return out
}

// The acceptance of #56: the dump exists before anything is dropped; then
// the role, the database and the PgBouncer entry are gone, and the
// account's usage is back to 0.
func TestDeleteDumpsThenDrops(t *testing.T) {
	p := newProvisioning(t)
	c := p.delete(p.running("production"))
	p.runOnce()

	want := outcome{Instance: "deleted", Op: "succeeded", Step: postgres.StepDropped, Payload: "{}"}
	if o := p.outcome(c); o != want {
		t.Fatalf("after the delete: %+v, want %+v", o, want)
	}
	name := c.Instance.InfraName
	if p.exists("role", name) || p.exists("database", name) {
		t.Errorf("role %t, database %t: want both gone", p.exists("role", name), p.exists("database", name))
	}
	if strings.Contains(p.cat(p.JailDeck().Client(), postgres.DatabasesFile), name) {
		t.Error("the PgBouncer entry is still there")
	}
	u, err := instances.AccountUsage(context.Background(), p.db.ConnAs(dbtest.RoleAPI), p.account)
	if err != nil || u != (instances.Usage{}) {
		t.Errorf("usage after the delete: %+v, %v", u, err)
	}

	ds := p.dumps(c)
	if len(ds) != 1 || ds[0].Expires < postgres.UserDeleteRetention-time.Minute || ds[0].Expires > postgres.UserDeleteRetention {
		t.Fatalf("final dumps: %+v, want one kept for 3 days", ds)
	}
	path := filepath.Join(p.driver.Dumper.Dir, ds[0].Ref)
	info, err := os.Stat(path)
	if err != nil || info.Size() != ds[0].Size || info.Mode().Perm() != 0o600 {
		t.Fatalf("dump file: %v, %v", info, err)
	}
	if err := p.driver.Dumper.Check(postgres.Dump{File: ds[0].Ref, Size: ds[0].Size, SHA256: ds[0].SHA256}); err != nil {
		t.Error(err)
	}
	// It is the customer's data: the table and its row are in it.
	argv := p.Docker([]string{p.driver.Dumper.Dir}, "pg_restore", "--data-only", "--file=-", path)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "keep me") {
		t.Errorf("pg_restore: %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(p.driver.Dumper.Dir); len(entries) != 1 {
		t.Errorf("dump directory holds %d files, want only the dump", len(entries))
	}
}

// A dump that fails drops nothing: the run is retried, the database is in
// place, and the next run, with pg_dump working, finishes the delete.
func TestDeleteKeepsTheDatabaseWhenTheDumpFails(t *testing.T) {
	p := newProvisioning(t)
	c := p.delete(p.running("production"))
	dumper := p.driver.Dumper
	p.driver.Dumper.Command = []string{"false"}
	p.runOnce()

	if o := p.outcome(c); o.Instance != "deleting" || o.Op != "pending" || o.Step != postgres.StepClosed {
		t.Fatalf("after a failed dump: %+v, want deleting, the operation pending at step closed", o)
	}
	name := c.Instance.InfraName
	if !p.exists("role", name) || !p.exists("database", name) || len(p.dumps(c)) != 0 {
		t.Fatalf("role %t, database %t, dumps %v: want both kept and no dump", p.exists("role", name), p.exists("database", name), p.dumps(c))
	}
	if entries, _ := os.ReadDir(dumper.Dir); len(entries) != 0 {
		t.Errorf("a failed dump left %d files", len(entries))
	}

	p.driver.Dumper = dumper
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE operations SET next_attempt_at = now() WHERE id = $1`, c.Operation.ID); err != nil {
		t.Fatal(err)
	}
	p.runOnce()
	if o := p.outcome(c); o.Instance != "deleted" || o.Op != "succeeded" || len(p.dumps(c)) != 1 || p.exists("database", name) {
		t.Errorf("after the retry: %+v, %d dumps, database %t", o, len(p.dumps(c)), p.exists("database", name))
	}
}

// What the worker did not make it leaves: a role a create refused stays,
// and the delete fails for an operator.
func TestDeleteLeavesWhatItDidNotMake(t *testing.T) {
	p := newProvisioning(t)
	c := p.create("production")
	ctx := context.Background()
	conn := p.admin()
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE ROLE "+c.Instance.InfraName+" LOGIN CREATEDB"); err != nil {
		t.Fatal(err)
	}
	p.runOnce() // the create fails
	c = p.delete(c)
	p.runOnce()
	if o := p.outcome(c); o.Instance != "failed" || o.InstanceReason != "delete_failed" || o.Op != "failed" {
		t.Errorf("after the delete: %+v, want it failed", o)
	}
	if !p.exists("role", c.Instance.InfraName) {
		t.Error("the role it did not make was dropped")
	}
}
