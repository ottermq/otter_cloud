package main

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
	"github.com/ottermq/otter_cloud/internal/instances"
)

// The acceptance of #102 through the commands: retry only a failed
// operation, cancel only a pending one, each audited, and last_error shown
// here.
func TestAdminOps(t *testing.T) {
	db := dbtest.New(t)
	withEnv(t, adminEnv(t, db))
	ctx := context.Background()
	api, admin, owner := db.ConnAs(dbtest.RoleAPI), db.ConnAs(dbtest.RoleAdmin), db.ConnAs(dbtest.RoleMigrator)
	if _, err := instances.AddNode(ctx, admin, instances.Node{Name: "node1", CapacityMemoryMB: 4096, CapacityStorageGB: 100,
		TenantSubnet: netip.MustParsePrefix("10.64.0.0/22"), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.AddCluster(ctx, admin, instances.Cluster{Name: "pgc1", Node: "node1", PGMajor: 18, JailName: "otc-pgc1",
		PoolAddr: netip.MustParseAddr("10.64.0.10"), PoolPort: 6432, MemoryMB: 2048, CapacityStorageGB: 20,
		CapacityConnections: 100, CapacityDatabases: 8}); err != nil {
		t.Fatal(err)
	}
	u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Subscribe(ctx, admin, u.AccountID, "founder-beta"); err != nil {
		t.Fatal(err)
	}
	create := func(name string) (instance, op string) {
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c, err := instances.Create(ctx, tx, instances.CreateRequest{AccountID: u.AccountID, RequestedBy: "user:" + u.ID,
			Name: name, FlavorCode: "db-small"})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return c.Instance.ID, c.Operation.ID
	}
	exec := func(sql string, args ...any) {
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := run(append([]string{"admin"}, args...), &out, &errOut)
		return out.String(), errOut.String(), code
	}

	// "prod": created, then its delete failed for good. "next": queued.
	prod, made := create("prod")
	exec(`UPDATE operations SET status = 'succeeded', finished_at = now(), payload = '{}' WHERE id = $1`, made)
	exec(`UPDATE instances SET status = 'failed', reason = 'delete_failed', desired_state = 'deleted' WHERE id = $1`, prod)
	var del string
	if err := owner.QueryRow(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by, status, attempts, step, reason, last_error, finished_at)
		SELECT id, account_id, node_id, 'delete', 'system', 'failed', 5, 'closed', 'delete_failed',
			'pg_dump otc_x: exec: "pg_dump": executable file not found in $PATH', now()
		FROM instances WHERE id = $1 RETURNING id`, prod).Scan(&del); err != nil {
		t.Fatal(err)
	}
	_, next := create("next")

	if out, errOut, code := run("instances", "list"); code != 0 || !strings.Contains(out, "ana@example.com") ||
		!strings.Contains(out, "prod") || !strings.Contains(out, "delete_failed") || !strings.Contains(out, "pgc1") {
		t.Errorf("instances list = %d %q %q", code, out, errOut)
	}
	if out, errOut, code := run("ops", "list", "-status", "failed"); code != 0 || !strings.Contains(out, del) ||
		!strings.Contains(out, "executable file not found") || strings.Contains(out, next) {
		t.Errorf("ops list -status failed = %d %q %q", code, out, errOut)
	}

	if _, errOut, code := run("ops", "retry", next); code != 1 || !strings.Contains(errOut, "only a failed operation") {
		t.Errorf("retry a pending one = %d %q", code, errOut)
	}
	if _, errOut, code := run("ops", "cancel", del); code != 1 || !strings.Contains(errOut, "only a pending operation") {
		t.Errorf("cancel a failed one = %d %q", code, errOut)
	}
	if _, errOut, code := run("ops", "retry"); code != 2 || !strings.Contains(errOut, "operation id") {
		t.Errorf("retry without an id = %d %q", code, errOut)
	}
	if out, errOut, code := run("ops", "retry", "-reason", "pg_dump on PATH now", del); code != 0 || !strings.Contains(out, "back in the queue") {
		t.Fatalf("retry = %d %q %q", code, out, errOut)
	}
	if out, errOut, code := run("ops", "cancel", next); code != 0 || !strings.Contains(out, "canceled") {
		t.Fatalf("cancel = %d %q %q", code, out, errOut)
	}
	if out, _, code := run("instances", "list"); code != 0 || strings.Contains(out, "next") || !strings.Contains(out, "deleting") {
		t.Errorf("instances list after = %d %q", code, out)
	}
	if out, _, code := run("instances", "list", "-all"); code != 0 || !strings.Contains(out, "next") {
		t.Errorf("instances list -all = %d %q", code, out)
	}

	var actions string
	if err := api.QueryRow(ctx, `
		SELECT string_agg(action || ':' || (details->>'kind') || ':' || coalesce(details->>'reason', '') || ':' || (account_id = $1), ',' ORDER BY at, id)
		FROM audit_events WHERE target_type = 'operation'`, u.AccountID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	// The refusals changed nothing and wrote no row.
	if want := "operation.retry:delete:pg_dump on PATH now:true,operation.cancel:create::true"; actions != want {
		t.Errorf("audit = %q\nwant    %q", actions, want)
	}
}
