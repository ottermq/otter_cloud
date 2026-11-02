package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

func TestNextStorageState(t *testing.T) {
	const gb = 1000
	for _, tc := range []struct {
		cur     string
		used    int64
		cleanup bool
		want    string
	}{
		{"ok", 799, false, "ok"},
		{"ok", 800, false, "warning"},
		{"warning", 999, false, "warning"},
		{"warning", 1000, false, "read_only"},
		{"ok", 1200, false, "blocked"},
		{"read_only", 950, false, "read_only"}, // under 100 %, not yet under 95 %
		{"read_only", 949, false, "warning"},
		{"read_only", 100, false, "ok"},
		{"read_only", 1300, true, "read_only"}, // reopened for a cleanup
		{"read_only", 1300, false, "blocked"},  // the window is over
		{"blocked", 10, false, "blocked"},      // only an operator reopens it
	} {
		if got := postgres.NextStorageState(tc.cur, tc.used, gb, tc.cleanup); got != tc.want {
			t.Errorf("%s at %d/%d (cleanup %t) = %s, want %s", tc.cur, tc.used, gb, tc.cleanup, got, tc.want)
		}
	}
}

// at makes the database's size the share r of its allocation, by shrinking
// what an allocated GB is: a test can't fill 2 GB.
func (p *provisioning) at(c instances.Created, r float64) {
	p.t.Helper()
	conn := p.admin()
	defer conn.Close(context.Background())
	var size int64
	if err := conn.QueryRow(context.Background(), `SELECT pg_database_size($1)`, c.Instance.InfraName).Scan(&size); err != nil {
		p.t.Fatal(err)
	}
	p.driver.BytesPerGB = int64(float64(size) / (float64(c.Instance.StorageGB) * r))
}

func (p *provisioning) checkStorage() map[string]operations.Storage {
	p.t.Helper()
	ss, err := p.driver.CheckStorage(context.Background(), p.db.PoolAs(p.db.NodeRole("node-a")), time.Now())
	if err != nil {
		p.t.Fatalf("check storage: %v", err)
	}
	out := map[string]operations.Storage{}
	for _, s := range ss {
		out[s.InstanceID] = s
	}
	return out
}

func (p *provisioning) view(c instances.Created) instances.InstanceView {
	p.t.Helper()
	v, err := instances.InstanceByID(context.Background(), p.db.ConnAs(dbtest.RoleAPI), p.account, c.Instance.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	return v
}

// write reports whether the tenant can insert through PgBouncer, and the
// error if not.
func (p *provisioning) write(c instances.Created, sql string) error {
	p.t.Helper()
	conn, err := p.viaPool(c)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(context.Background(), sql)
	return err
}

// The acceptance of #99, each step of Q27 on the test cluster: a warning
// at 80 %; read-only at 100 %, which BEGIN READ WRITE gets round to delete
// data; read-write again under 95 %, not before; connections refused at
// 120 %, at PgBouncer and in PostgreSQL, and start refused; an operator's
// reopen lets it in read-only until the window ends.
func TestStorageQuota(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	step := func(want string) operations.Storage {
		t.Helper()
		s, ok := p.checkStorage()[c.Instance.ID]
		if !ok || s.To != want {
			t.Fatalf("storage %+v, want %s", s, want)
		}
		if v := p.view(c); v.Storage.State != want || v.Storage.UsedBytes == nil || *v.Storage.UsedBytes != s.Used {
			t.Errorf("view storage %+v, want %s at %d", v.Storage, want, s.Used)
		}
		return s
	}

	p.at(c, 0.5)
	step("ok")
	p.at(c, 0.85)
	step("warning")
	if err := p.write(c, `INSERT INTO notes VALUES ('fine')`); err != nil {
		t.Errorf("write at a warning: %v", err)
	}

	p.at(c, 1.05)
	step("read_only")
	if v := p.view(c); v.Reason != "over_storage_quota" {
		t.Errorf("reason %q, want over_storage_quota", v.Reason)
	}
	if err := p.write(c, `INSERT INTO notes VALUES ('no')`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("write when read-only: %v, want refused", err)
	}
	if err := p.write(c, `BEGIN READ WRITE; DELETE FROM notes WHERE body = 'fine'; COMMIT`); err != nil {
		t.Errorf("delete with BEGIN READ WRITE: %v", err)
	}
	p.at(c, 0.97)
	step("read_only")
	p.at(c, 0.9)
	step("warning")
	if err := p.write(c, `INSERT INTO notes VALUES ('again')`); err != nil {
		t.Errorf("write back under 95 %%: %v", err)
	}

	p.at(c, 1.25)
	step("blocked")
	if v := p.view(c); v.Reason != "storage_blocked" {
		t.Errorf("reason %q, want storage_blocked", v.Reason)
	}
	if _, err := p.viaPool(c); err == nil {
		t.Error("a blocked database takes connections through PgBouncer")
	}
	if err := p.adminTo(c); err == nil {
		t.Error("a blocked database takes connections in PostgreSQL")
	}
	if strings.Contains(p.cat(p.JailDeck().Client(), postgres.DatabasesFile), c.Instance.InfraName) {
		t.Error("a blocked database has a PgBouncer entry")
	}
	if hs, err := p.driver.CheckHealth(context.Background(), p.worker.DB); err != nil || len(hs) != 0 {
		t.Errorf("health checks of a blocked database: %+v, %v; want none", hs, err)
	}
	p.at(c, 0.5)
	step("blocked") // only an operator reopens it

	tx, err := p.db.ConnAs(dbtest.RoleAdmin).Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := instances.Reopen(context.Background(), tx, c.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(r.CleanupUntil); d < instances.CleanupWindow-time.Minute || d > instances.CleanupWindow {
		t.Errorf("cleanup until %s, want %s from now", r.CleanupUntil, instances.CleanupWindow)
	}
	p.at(c, 1.25)
	step("read_only")
	if err := p.write(c, `INSERT INTO notes VALUES ('no')`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("write while reopened: %v, want refused", err)
	}
	if err := p.write(c, `BEGIN READ WRITE; DELETE FROM notes; COMMIT`); err != nil {
		t.Errorf("clean up while reopened: %v", err)
	}
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE instances SET cleanup_until = now() - interval '1 second' WHERE id = $1`, c.Instance.ID); err != nil {
		t.Fatal(err)
	}
	p.at(c, 1.25)
	step("blocked") // the window is over, and it is still over

	var n int
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'instance.storage' AND actor = 'system' AND target_id = $1`,
		c.Instance.ID).Scan(&n); err != nil || n != 5 {
		t.Errorf("%d instance.storage events, %v; want 5, one per change the worker made", n, err)
	}
}

// An operation in flight owns the database: the check measures it and
// changes nothing.
func TestStorageQuotaSkipsAnOperationInFlight(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.request("restart", c)
	p.at(c, 1.25)
	if s := p.checkStorage()[c.Instance.ID]; s.To != "ok" || s.Used == 0 {
		t.Errorf("storage %+v, want measured and still ok", s)
	}
	if err := p.adminTo(c); err != nil {
		t.Errorf("connections refused while an operation is in flight: %v", err)
	}
}
