package operations_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
)

type fixture struct {
	t                      *testing.T
	db                     *dbtest.DB
	accounts               int
	lastAccount, lastActor string
}

// newFixture is a database with the given nodes, each with one cluster
// named after it.
func newFixture(t *testing.T, nodes ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, db: dbtest.New(t)}
	for i, n := range nodes {
		f.addNode(n, i)
	}
	return f
}

func (f *fixture) addNode(name string, i int) {
	f.t.Helper()
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	if _, err := instances.AddNode(ctx, admin, instances.Node{Name: name, CapacityMemoryMB: 65536, CapacityStorageGB: 1000,
		TenantSubnet: netip.MustParsePrefix(fmt.Sprintf("10.64.%d.0/24", i)), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := instances.AddCluster(ctx, admin, instances.Cluster{Name: "pg-" + name, Node: name, PGMajor: 18, JailName: "otc-pg-" + name,
		PoolAddr: netip.MustParseAddr(fmt.Sprintf("10.64.%d.10", i)), PoolPort: 6432, MemoryMB: 2048,
		CapacityStorageGB: 500, CapacityConnections: 3000, CapacityDatabases: 100}); err != nil {
		f.t.Fatal(err)
	}
}

// only leaves the cluster of node the only one placement may choose.
func (f *fixture) only(node string, all ...string) {
	f.t.Helper()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	for _, n := range all {
		status := instances.StatusDraining
		if n == node {
			status = instances.StatusActive
		}
		if _, err := instances.SetClusterStatus(context.Background(), admin, "pg-"+n, status); err != nil {
			f.t.Fatal(err)
		}
	}
}

// creates queues n create operations, through the API's own path, and
// returns their ids.
func (f *fixture) creates(n int) []string {
	f.t.Helper()
	ctx := context.Background()
	api := f.db.ConnAs(dbtest.RoleAPI)
	var ids []string
	for len(ids) < n {
		if len(ids)%2 == 0 { // two per account, the Founder Beta's quota
			f.accounts++
			u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: fmt.Sprintf("c%d@example.com", f.accounts), Password: "correct horse battery staple"})
			if err != nil {
				f.t.Fatal(err)
			}
			if _, err := billing.Subscribe(ctx, f.db.ConnAs(dbtest.RoleAdmin), u.AccountID, "founder-beta"); err != nil {
				f.t.Fatal(err)
			}
			f.lastAccount, f.lastActor = u.AccountID, "user:"+u.ID
		}
		tx, err := api.Begin(ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		c, err := instances.Create(ctx, tx, instances.CreateRequest{AccountID: f.lastAccount, RequestedBy: f.lastActor,
			Name: fmt.Sprintf("db-%d", len(ids)), FlavorCode: "db-small"})
		if err != nil {
			f.t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, c.Operation.ID)
	}
	return ids
}

// worker is a connection as node's worker role.
func (f *fixture) worker(node string) *pgx.Conn {
	f.t.Helper()
	return f.db.ConnAs(f.db.NodeRole(node))
}

type opRow struct {
	Status, Step, Reason, LastError, Payload string
	Attempts                                 int
	Locked                                   bool
	Due                                      time.Duration // next_attempt_at - now()
}

func (f *fixture) op(id string) opRow {
	f.t.Helper()
	var r opRow
	var ms float64
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT status, coalesce(step, ''), coalesce(reason, ''), coalesce(last_error, ''), payload::text, attempts,
			locked_by IS NOT NULL, extract(epoch FROM next_attempt_at - now()) * 1000
		FROM operations WHERE id = $1`, id).
		Scan(&r.Status, &r.Step, &r.Reason, &r.LastError, &r.Payload, &r.Attempts, &r.Locked, &ms); err != nil {
		f.t.Fatal(err)
	}
	r.Due = time.Duration(ms) * time.Millisecond
	return r
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

var creates = []string{"create"}

// The acceptance of #47: a worker can't read another node's rows, or
// claim or change its operations.
func TestWorkerSeesOnlyItsNode(t *testing.T) {
	f := newFixture(t, "node-a", "node-b")
	ctx := context.Background()
	f.only("node-a", "node-a", "node-b")
	onA := f.creates(1)[0]
	f.only("node-b", "node-a", "node-b")
	onB := f.creates(1)[0]

	a := f.worker("node-a")
	node, err := operations.NodeID(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"infrastructure_nodes", "clusters", "instances", "operations"} {
		var n int
		if err := a.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 1 {
			t.Errorf("node-a sees %d rows of %s, %v; want 1", n, table, err)
		}
		if err := f.db.ConnAs(dbtest.RoleAPI).QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 2 {
			t.Errorf("the api sees %d rows of %s, %v; want 2", n, table, err)
		}
	}
	var name string
	if err := a.QueryRow(ctx, `SELECT name FROM infrastructure_nodes WHERE id = $1`, node).Scan(&name); err != nil || name != "node-a" {
		t.Errorf("NodeID = node %q, %v", name, err)
	}

	op, ok, err := operations.Claim(ctx, a, "w1", creates, time.Minute)
	if err != nil || !ok || op.ID != onA || op.NodeID != node {
		t.Fatalf("claim = %+v, %v, %v; want %s", op, ok, err, onA)
	}
	if _, ok, err := operations.Claim(ctx, a, "w1", creates, time.Minute); ok || err != nil {
		t.Errorf("node-a claimed node-b's operation, %v", err)
	}
	if tag, err := a.Exec(ctx, `UPDATE operations SET step = 'x', status = 'running' WHERE id = $1`, onB); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("node-a changed node-b's operation: %v, %v", tag, err)
	}
	if f.op(onB).Status != "pending" {
		t.Error("node-b's operation is no longer pending")
	}

	// otter_worker itself, or any role that is no node's, sees nothing.
	if _, err := operations.NodeID(ctx, f.db.ConnAs(dbtest.RoleWorker)); !errors.Is(err, operations.ErrNotAWorker) {
		t.Errorf("NodeID as otter_worker = %v", err)
	}
	var n int
	if err := f.db.ConnAs(dbtest.RoleWorker).QueryRow(ctx, `SELECT count(*) FROM operations`).Scan(&n); err != nil || n != 0 {
		t.Errorf("otter_worker sees %d operations, %v", n, err)
	}
}

func TestWorkerPrivileges(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE operations SET kind = 'delete'`,
		`UPDATE operations SET node_id = node_id`,
		`UPDATE operations SET requested_by = 'system'`,
		`INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by) SELECT instance_id, account_id, node_id, 'delete', 'system' FROM operations`,
		`DELETE FROM operations`,
		`UPDATE instances SET storage_gb = 1000`,
		// The observed state only (#134): never what the customer asked for.
		`UPDATE instances SET desired_state = 'deleted'`,
		`UPDATE instances SET account_id = account_id`,
		`UPDATE instances SET endpoint_host = 'db.example.com'`,
		`UPDATE infrastructure_nodes SET status = 'active'`,
		`SELECT 1 FROM accounts`,
		`SELECT 1 FROM subscriptions`,
		`SELECT 1 FROM audit_events`,
	} {
		_, err := f.worker("node-a").Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s = %v, want permission denied", sql, err)
		}
	}
	if f.op(id).Status != "pending" {
		t.Error("the operation changed")
	}
}

func TestNamesMatchTheDatabase(t *testing.T) {
	f := newFixture(t, "node-a")
	var role, channel, id string
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT node_role(name), ops_channel(id), id FROM infrastructure_nodes`).Scan(&role, &channel, &id); err != nil {
		t.Fatal(err)
	}
	if role != instances.NodeRole("node-a") || role != f.db.NodeRole("node-a") || channel != operations.Channel(id) {
		t.Errorf("node_role %q, ops_channel %q; Go says %q, %q", role, channel, instances.NodeRole("node-a"), operations.Channel(id))
	}
}

// The acceptance of #47: two workers never run one operation.
func TestConcurrentClaims(t *testing.T) {
	f := newFixture(t, "node-a")
	ids := f.creates(12)
	ctx := context.Background()

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := range 4 {
		conn := f.worker("node-a")
		wg.Go(func() {
			for {
				op, ok, err := operations.Claim(ctx, conn, fmt.Sprintf("w%d", w), creates, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[op.ID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(claimed) != len(ids) {
		t.Errorf("claimed %d of %d operations", len(claimed), len(ids))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("operation %s claimed %d times", id, n)
		}
	}
}

// The acceptance of #47: an expired lease is reclaimed, and the worker
// that held it can no longer touch the operation.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	ctx := context.Background()
	conn := f.worker("node-a")

	first, ok, err := operations.Claim(ctx, conn, "w1", creates, 50*time.Millisecond)
	if err != nil || !ok || first.Attempts != 1 {
		t.Fatalf("claim = %+v, %v, %v", first, ok, err)
	}
	if err := operations.Checkpoint(ctx, conn, &first, "role_created"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := operations.Claim(ctx, conn, "w2", creates, time.Minute); ok {
		t.Fatal("claimed while the lease holds")
	}
	time.Sleep(100 * time.Millisecond)

	second, ok, err := operations.Claim(ctx, conn, "w1", creates, time.Minute)
	if err != nil || !ok || second.ID != id || second.Attempts != 2 || second.Step != "role_created" || second.Lock == first.Lock {
		t.Fatalf("reclaim = %+v, %v, %v", second, ok, err)
	}
	for name, err := range map[string]error{
		"renew":      operations.Renew(ctx, conn, first, time.Minute),
		"checkpoint": operations.Checkpoint(ctx, conn, &first, "database_created"),
		"succeed":    operations.Succeed(ctx, conn, first),
	} {
		if !errors.Is(err, operations.ErrLeaseLost) {
			t.Errorf("%s with the old claim = %v, want ErrLeaseLost", name, err)
		}
	}
	if err := operations.Succeed(ctx, conn, second); err != nil {
		t.Fatal(err)
	}
	if r := f.op(id); r.Status != "succeeded" || r.Locked || r.Step != "role_created" {
		t.Errorf("after succeed: %+v", r)
	}
}

func TestRetryBacksOffThenFails(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	ctx := context.Background()
	conn := f.worker("node-a")
	f.exec(`UPDATE operations SET payload = '{"verifier": "SCRAM-SHA-256$…"}' WHERE id = $1`, id)

	for attempt := 1; attempt <= operations.MaxAttempts; attempt++ {
		op, ok, err := operations.Claim(ctx, conn, "w1", creates, time.Minute)
		if err != nil || !ok || op.Attempts != attempt {
			t.Fatalf("attempt %d: claim = %+v, %v, %v", attempt, op, ok, err)
		}
		failed, err := operations.Retry(ctx, conn, op, fmt.Errorf("cluster pg-node-a: connection refused (%d)", attempt))
		if err != nil {
			t.Fatal(err)
		}
		r := f.op(id)
		if attempt < operations.MaxAttempts {
			want := operations.Backoff(attempt)
			if failed || r.Status != "pending" || r.Locked || r.Due < want-5*time.Second || r.Due > want {
				t.Fatalf("attempt %d: failed %t, %+v; want pending, due in %v", attempt, failed, r, want)
			}
			if _, ok, _ := operations.Claim(ctx, conn, "w1", creates, time.Minute); ok {
				t.Fatalf("attempt %d: claimed during the backoff", attempt)
			}
			f.exec(`UPDATE operations SET next_attempt_at = now() WHERE id = $1`, id)
			continue
		}
		if !failed || r.Status != "failed" || r.Reason != "create_failed" || r.Payload != "{}" ||
			!strings.Contains(r.LastError, "connection refused (5)") || r.Locked {
			t.Errorf("after the last attempt: failed %t, %+v", failed, r)
		}
		// A failed create leaves the instance failed, with the reason.
		var status, reason string
		if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(ctx, `SELECT i.status, coalesce(i.reason, '') FROM instances i
			JOIN operations o ON o.instance_id = i.id WHERE o.id = $1`, id).Scan(&status, &reason); err != nil ||
			status != "failed" || reason != "create_failed" {
			t.Errorf("instance after the last attempt: %s %q, %v", status, reason, err)
		}
	}
	if got := []time.Duration{operations.Backoff(1), operations.Backoff(2), operations.Backoff(7), operations.Backoff(30)}; got[0] != 10*time.Second ||
		got[1] != 20*time.Second || got[2] != 10*time.Minute || got[3] != 10*time.Minute {
		t.Errorf("backoffs = %v", got)
	}
}

func TestFailRefusesAnUnlistedReason(t *testing.T) {
	f := newFixture(t, "node-a")
	f.creates(1)
	ctx := context.Background()
	conn := f.worker("node-a")
	op, _, err := operations.Claim(ctx, conn, "w1", creates, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := operations.Fail(ctx, conn, op, "cluster_down", errors.New("x")); err == nil {
		t.Error("failed with a reason that isn't on the list")
	}
}
