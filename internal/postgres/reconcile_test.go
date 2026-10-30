package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

func (p *provisioning) reconcile() []string {
	p.t.Helper()
	fs, err := p.driver.Reconcile(context.Background(), p.worker.DB)
	if err != nil {
		p.t.Fatalf("reconcile: %v", err)
	}
	var kinds []string
	for _, f := range fs {
		// The cluster outlives the tests: another test's leftover is not
		// this one's finding.
		if f.Kind == "unknown_database" && f.Subject != unknownDB {
			continue
		}
		kinds = append(kinds, f.Kind+":"+f.Subject)
	}
	return kinds
}

// unknownDB is the database TestReconcileUnknownDatabase makes.
const unknownDB = "otc_ffffffffffff"

func (p *provisioning) adminExec(sql string) {
	p.t.Helper()
	conn := p.admin()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		p.t.Fatal(err)
	}
}

func (p *provisioning) instance(c instances.Created) (status, reason string) {
	p.t.Helper()
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT status, coalesce(reason, '') FROM instances WHERE id = $1`, c.Instance.ID).Scan(&status, &reason); err != nil {
		p.t.Fatal(err)
	}
	return status, reason
}

// What matches the platform DB is left as it is, and reported as nothing.
func TestReconcileQuiet(t *testing.T) {
	p := newProvisioning(t)
	running := p.running("production")
	stopped := p.running("staging")
	p.request("stop", stopped)
	p.runOnce()
	if got := p.reconcile(); len(got) != 0 {
		t.Errorf("findings = %v, want none", got)
	}
	if s, _ := p.instance(running); s != "running" {
		t.Errorf("running is %s", s)
	}
	if s, _ := p.instance(stopped); s != "stopped" {
		t.Errorf("stopped is %s", s)
	}
}

// The acceptance of #96: a database dropped behind the platform's back →
// failed and an alert, never recreated; reported once.
func TestReconcileDatabaseMissing(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.adminExec("DROP DATABASE " + c.Instance.InfraName + " WITH (FORCE)")
	if got := p.reconcile(); !slices.Equal(got, []string{"database_missing:" + c.Instance.InfraName}) {
		t.Errorf("findings = %v", got)
	}
	if s, r := p.instance(c); s != "failed" || r != postgres.ReasonMissing {
		t.Errorf("instance %s, %s; want failed, %s", s, r, postgres.ReasonMissing)
	}
	if p.exists("database", c.Instance.InfraName) {
		t.Error("the database was recreated")
	}
	if got := p.reconcile(); len(got) != 0 {
		t.Errorf("second run: %v, want nothing new", got)
	}
	if instances.ReasonMessage(postgres.ReasonMissing) == "Something went wrong. Contact support." {
		t.Error("the reason is not on the list")
	}
}

// The acceptance of #96: an orphan of a deleted row → delete queued, which
// dumps it first and then drops it.
func TestReconcileOrphanOfADeletedRow(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), `
		UPDATE instances SET status = 'deleted', desired_state = 'deleted', deleted_at = now() WHERE id = $1`, c.Instance.ID); err != nil {
		t.Fatal(err)
	}
	got := p.reconcile()
	if !slices.Contains(got, "orphan_queued:"+c.Instance.InfraName) {
		t.Fatalf("findings = %v", got)
	}
	p.runOnce()
	var op, status string
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT o.id, o.status FROM operations o WHERE o.instance_id = $1 AND o.kind = 'delete'`, c.Instance.ID).Scan(&op, &status); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || p.exists("database", c.Instance.InfraName) || p.exists("role", c.Instance.InfraName) {
		t.Errorf("delete %s; database %t, role %t", status, p.exists("database", c.Instance.InfraName), p.exists("role", c.Instance.InfraName))
	}
	if ds := p.dumps(c); len(ds) != 1 {
		t.Errorf("%d final dumps, want 1", len(ds))
	}
	if s, _ := p.instance(c); s != "deleted" {
		t.Errorf("instance %s", s)
	}
	if got := p.reconcile(); len(got) != 0 {
		t.Errorf("after the delete: %v", got)
	}
}

// An orphan whose delete failed for good is reported, not queued again
// every five minutes, and its instance stays deleted.
func TestReconcileOrphanWhoseDeleteFailed(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), `
		UPDATE instances SET status = 'deleted', desired_state = 'deleted', deleted_at = now() WHERE id = $1`, c.Instance.ID); err != nil {
		t.Fatal(err)
	}
	p.reconcile()
	p.driver.Dumper.Command = []string{"false"}
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE operations SET attempts = $2 - 1 WHERE instance_id = $1 AND kind = 'delete'`, c.Instance.ID, operations.MaxAttempts); err != nil {
		t.Fatal(err)
	}
	p.runOnce()
	if s, _ := p.instance(c); s != "deleted" {
		t.Errorf("instance %s after its orphan's delete failed, want deleted", s)
	}
	if got := p.reconcile(); !slices.Equal(got, []string{"orphan_delete_failed:" + c.Instance.InfraName}) {
		t.Errorf("findings = %v", got)
	}
	var n int
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT count(*) FROM operations WHERE instance_id = $1 AND kind = 'delete'`, c.Instance.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d deletes queued, %v; want 1", n, err)
	}
	if !p.exists("database", c.Instance.InfraName) {
		t.Error("the database was dropped without its dump")
	}
}

// The acceptance of #96: an unknown otc_ database → alert only.
func TestReconcileUnknownDatabase(t *testing.T) {
	p := newProvisioning(t)
	const name = unknownDB
	p.adminExec("CREATE DATABASE " + name)
	t.Cleanup(func() { p.adminExec("DROP DATABASE IF EXISTS " + name) })
	if got := p.reconcile(); !slices.Equal(got, []string{"unknown_database:" + name}) {
		t.Errorf("findings = %v", got)
	}
	if !p.exists("database", name) {
		t.Error("the unknown database was dropped")
	}
}

// PgBouncer serving something else is put right: an entry holds no data.
func TestReconcileRepairsThePool(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	jd := p.JailDeck().Client()
	if _, err := postgres.EnsurePoolEntries(context.Background(), jd, postgres.Pool{Jail: "otc-pg-test"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := p.reconcile(); len(got) != 1 || !strings.HasPrefix(got[0], "pool_repaired:") {
		t.Errorf("findings = %v", got)
	}
	if !strings.Contains(p.cat(jd, postgres.DatabasesFile), c.Instance.InfraName) {
		t.Error("the entry is still missing")
	}
}

// An instance with an operation in flight is the operation's: a database
// gone while its stop is queued is left for the stop to find.
func TestReconcileSkipsAnOperationInFlight(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.request("stop", c)
	p.adminExec("DROP DATABASE " + c.Instance.InfraName + " WITH (FORCE)")
	if got := p.reconcile(); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "database_missing") }) {
		t.Errorf("findings = %v", got)
	}
	if s, _ := p.instance(c); s != "running" {
		t.Errorf("instance %s", s)
	}
}

// A cluster the worker can't reach is reported, and judged no further: an
// outage is not a loss.
func TestReconcileUnreachableCluster(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.driver.Port = 1
	got := p.reconcile()
	if len(got) != 1 || !strings.HasPrefix(got[0], "cluster_unreachable:") {
		t.Errorf("findings = %v", got)
	}
	if s, _ := p.instance(c); s != "running" {
		t.Errorf("instance %s, want running", s)
	}
}
