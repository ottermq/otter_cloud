package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// errKilled is what a killed worker's connection answers: nothing it does
// after the kill reaches the platform DB.
var errKilled = errors.New("the worker was killed")

// killer is the platform DB as a worker that dies at one moment: right
// after a statement matching at commits. From then on its context is
// canceled, so the handler's next call to a cluster or JailDeck fails,
// and every statement fails, so neither the handler nor the worker records
// anything: the operation stays running and locked, as after SIGKILL.
type killer struct {
	db     operations.DB
	at     func(sql string, args []any) bool
	kill   context.CancelFunc
	mu     sync.Mutex
	killed bool
}

func (k *killer) dead() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.killed
}

func (k *killer) after(sql string, args []any) {
	if k.at(sql, args) {
		k.mu.Lock()
		k.killed = true
		k.mu.Unlock()
		k.kill()
	}
}

func (k *killer) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if k.dead() {
		return pgconn.CommandTag{}, errKilled
	}
	tag, err := k.db.Exec(ctx, sql, args...)
	if err == nil {
		k.after(sql, args)
	}
	return tag, err
}

func (k *killer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if k.dead() {
		return nil, errKilled
	}
	return k.db.Query(ctx, sql, args...)
}

func (k *killer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if k.dead() {
		return errRow{}
	}
	return k.db.QueryRow(ctx, sql, args...)
}

type errRow struct{}

func (errRow) Scan(...any) error { return errKilled }

// atStep kills right after the checkpoint of step; atRecord right after the
// handler recorded the instance's outcome, before the operation is
// finished.
func atStep(step string) func(string, []any) bool {
	return func(sql string, args []any) bool {
		return strings.Contains(sql, "SET step = $3") && len(args) == 3 && args[2] == step
	}
}

func atRecord(status string) func(string, []any) bool {
	return func(sql string, _ []any) bool {
		return strings.HasPrefix(strings.TrimSpace(sql), "UPDATE instances SET status = '"+status+"'")
	}
}

// crashThenRestart runs the next operation on a worker that is killed at
// at, then lets its lease run out and runs it again on a fresh worker, as
// rc.d restarting it would.
func (p *provisioning) crashThenRestart(c instances.Created, at func(string, []any) bool) {
	p.t.Helper()
	ctx, kill := context.WithCancel(context.Background())
	defer kill()
	k := &killer{db: p.worker.DB, at: at, kill: kill}
	dying := *p.worker
	dying.DB = k
	if _, err := dying.RunOnce(ctx); !k.dead() {
		p.t.Fatalf("the worker was never killed: %v", err)
	}
	if o := p.outcome(c); o.Op != "running" {
		p.t.Fatalf("after the kill: %+v, want the operation still running, as a dead worker leaves it", o)
	}
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE operations SET locked_until = now() - interval '1 second' WHERE id = $1`, c.Operation.ID); err != nil {
		p.t.Fatal(err)
	}
	p.runOnce()
}

// The acceptance of #57 for create: killed after each step, the create
// completes on restart with one role, one database and one PgBouncer entry,
// and the customer's password works.
func TestCreateSurvivesAKillAtEachStep(t *testing.T) {
	for _, step := range []string{postgres.StepRole, postgres.StepDatabase, postgres.StepPool, postgres.StepCheck, "recorded"} {
		t.Run(step, func(t *testing.T) {
			p := newProvisioning(t)
			c := p.create("production")
			at := atStep(step)
			if step == "recorded" {
				at = atRecord("running")
			}
			p.crashThenRestart(c, at)

			if o := p.outcome(c); o.Instance != "running" || o.Op != "succeeded" || o.Payload != "{}" {
				t.Fatalf("after the restart: %+v", o)
			}
			var attempts int
			if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
				`SELECT attempts FROM operations WHERE id = $1`, c.Operation.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Errorf("attempts = %d, %v; want 2: the killed run and the one after", attempts, err)
			}
			p.noDuplicates(c)
			db, err := p.asTenantVia("pool-port", postgres.Tenant{Name: c.Instance.InfraName}, c.Password)
			if err != nil {
				t.Fatalf("log in after the restart: %v", err)
			}
			_ = db.Close(context.Background())
		})
	}
}

// The acceptance of #57 for delete: killed after each step, and once in
// the middle of the dump, the delete completes on restart with exactly one
// dump, recorded once, nothing else left in the dump directory, and
// nothing left on the cluster.
func TestDeleteSurvivesAKillAtEachStep(t *testing.T) {
	steps := []string{postgres.StepUnrouted, postgres.StepClosed, "mid-dump", postgres.StepDumped, postgres.StepDropped, "recorded"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			p := newProvisioning(t)
			c := p.delete(p.running("production"))
			at := atStep(step)
			switch step {
			case "recorded":
				at = atRecord("deleted")
			case "mid-dump":
				// SIGKILL during pg_dump leaves its temporary files: the
				// restart must not trip over them or leave them behind.
				name := c.Instance.InfraName + "-" + c.Operation.ID + ".dump"
				for _, f := range []string{"." + name + ".tmp", "." + name + ".root.crt"} {
					if err := os.WriteFile(filepath.Join(p.driver.Dumper.Dir, f), []byte("half"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				at = atStep(postgres.StepClosed)
			}
			p.crashThenRestart(c, at)

			if o := p.outcome(c); o.Instance != "deleted" || o.Op != "succeeded" {
				t.Fatalf("after the restart: %+v", o)
			}
			name := c.Instance.InfraName
			if p.exists("role", name) || p.exists("database", name) ||
				strings.Contains(p.cat(p.JailDeck().Client(), postgres.DatabasesFile), name) {
				t.Errorf("left on the cluster: role %t, database %t", p.exists("role", name), p.exists("database", name))
			}
			ds := p.dumps(c)
			if len(ds) != 1 {
				t.Fatalf("%d final dumps recorded, want 1", len(ds))
			}
			if err := p.driver.Dumper.Check(postgres.Dump{File: ds[0].Ref, Size: ds[0].Size, SHA256: ds[0].SHA256}); err != nil {
				t.Error(err)
			}
			entries, _ := os.ReadDir(p.driver.Dumper.Dir)
			if len(entries) != 1 || entries[0].Name() != ds[0].Ref {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("dump directory holds %v, want only %s", names, ds[0].Ref)
			}
		})
	}
}

// noDuplicates checks the cluster holds one of each thing the create made.
func (p *provisioning) noDuplicates(c instances.Created) {
	p.t.Helper()
	ctx := context.Background()
	conn := p.admin()
	defer conn.Close(ctx)
	var roles, dbs int
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_roles WHERE rolname = $1), (SELECT count(*) FROM pg_database WHERE datname = $1)`,
		c.Instance.InfraName).Scan(&roles, &dbs); err != nil || roles != 1 || dbs != 1 {
		p.t.Errorf("%d roles, %d databases named %s, %v; want one each", roles, dbs, c.Instance.InfraName, err)
	}
	if n := strings.Count(p.cat(p.JailDeck().Client(), postgres.DatabasesFile), c.Instance.InfraName+" = "); n != 1 {
		p.t.Errorf("%d PgBouncer entries for %s, want 1", n, c.Instance.InfraName)
	}
}
