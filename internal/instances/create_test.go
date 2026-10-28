package instances_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/scram"
)

type fixture struct {
	t  *testing.T
	db *dbtest.DB
}

// newFixture is a node with one cluster, pgc1.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, db: dbtest.New(t)}
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	if _, err := instances.AddNode(context.Background(), admin, node("node-01", "10.64.0.0/22")); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.AddCluster(context.Background(), admin, cluster("pgc1", "node-01", "10.64.0.10")); err != nil {
		t.Fatal(err)
	}
	return f
}

// customer is a user with an account, subscribed to the Founder Beta
// unless subscribe is false. It returns the account id.
func (f *fixture) customer(email string, subscribe bool) (string, string) {
	f.t.Helper()
	ctx := context.Background()
	u, err := identity.CreateUser(ctx, f.db.ConnAs(dbtest.RoleAPI), identity.NewUser{Email: email, Password: "correct horse battery staple"})
	if err != nil {
		f.t.Fatal(err)
	}
	if subscribe {
		if _, err := billing.Subscribe(ctx, f.db.ConnAs(dbtest.RoleAdmin), u.AccountID, "founder-beta"); err != nil {
			f.t.Fatal(err)
		}
	}
	return u.AccountID, "user:" + u.ID
}

// create runs Create in its own transaction as otter_api, and commits on
// success.
func create(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, r instances.CreateRequest) (instances.Created, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return instances.Created{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := instances.Create(ctx, tx, r)
	if err != nil {
		return instances.Created{}, err
	}
	return c, tx.Commit(ctx)
}

func (f *fixture) create(account, actor, name, flavor string) (instances.Created, error) {
	f.t.Helper()
	return create(context.Background(), f.db.ConnAs(dbtest.RoleAPI),
		instances.CreateRequest{AccountID: account, Name: name, FlavorCode: flavor, RequestedBy: actor})
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	c, err := f.create(account, actor, "production", "db-small")
	if err != nil {
		t.Fatal(err)
	}
	in := c.Instance
	if in.ID == "" || in.Status != "provisioning" || in.DesiredState != "running" || in.Health != "unknown" ||
		in.StorageGB != 2 || in.MaxConnections == nil || *in.MaxConnections != 20 || in.ClusterID == "" ||
		!strings.HasPrefix(in.InfraName, "otc_") || len(in.InfraName) != 16 || c.Replayed {
		t.Errorf("instance = %+v", in)
	}
	if c.Operation.Kind != "create" || c.Operation.Status != "pending" || c.Operation.InstanceID != in.ID {
		t.Errorf("operation = %+v", c.Operation)
	}
	var node, by string
	if err := f.db.ConnAs(dbtest.RoleAPI).QueryRow(context.Background(),
		`SELECT n.name, o.requested_by FROM operations o JOIN infrastructure_nodes n ON n.id = o.node_id WHERE o.id = $1`,
		c.Operation.ID).Scan(&node, &by); err != nil || node != "node-01" || by != actor {
		t.Errorf("operation for node %q by %q, %v", node, by, err)
	}
	// The password is returned; only its verifier is kept, for the worker.
	var payload map[string]string
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT payload FROM operations WHERE id = $1`, c.Operation.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if len(c.Password) != scram.PasswordLength || len(payload) != 1 ||
		!strings.HasPrefix(payload["verifier"], "SCRAM-SHA-256$4096:") || strings.Contains(payload["verifier"], c.Password) {
		t.Errorf("password %d characters, payload %v", len(c.Password), payload)
	}
}

// The acceptance of #44, first part: no subscription → refused (403 in the API).
func TestCreateNeedsEntitlement(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", false)
	if _, err := f.create(account, actor, "production", "db-small"); !errors.Is(err, instances.ErrNotEntitled) {
		t.Errorf("no subscription: %v, want ErrNotEntitled", err)
	}

	paying, actor2 := f.customer("bob@example.com", false)
	if _, err := billing.SyncPlans(context.Background(), f.db.ConnAs(dbtest.RoleMigrator), []billing.Plan{
		{Code: "founder-beta", Name: "Founder Beta", Currency: "BRL", Interval: "none", Limits: billing.Limits{Instances: 2, StorageGB: 10, Connections: 60}},
		{Code: "pro", Name: "Pro", PriceMinor: 4900, Currency: "BRL", Interval: "month", Limits: billing.Limits{Instances: 5, StorageGB: 50, Connections: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Subscribe(context.Background(), f.db.ConnAs(dbtest.RoleAdmin), paying, "pro"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(paying, actor2, "production", "db-small"); !errors.Is(err, instances.ErrNotEntitled) {
		t.Errorf("paid plan, payment pending: %v, want ErrNotEntitled", err)
	}

	if _, err := f.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE accounts SET status = 'suspended' WHERE id = $1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(account, actor, "production", "db-small"); !errors.Is(err, instances.ErrAccountNotActive) {
		t.Errorf("suspended account: %v", err)
	}
}

// The acceptance of #44, second part: over quota → refused (409), naming the limit.
func TestCreateOverQuotaNamesTheLimit(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	mig := f.db.ConnAs(dbtest.RoleMigrator)
	ctx := context.Background()
	quota := func(err error) string {
		t.Helper()
		var q *instances.QuotaError
		if !errors.As(err, &q) {
			t.Fatalf("err = %v, want a QuotaError", err)
		}
		return q.Limit
	}

	// Founder Beta: 2 databases, 10 GB, 60 connections. Two medium fill it.
	for _, name := range []string{"one", "two"} {
		if _, err := f.create(account, actor, name, "db-medium"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.create(account, actor, "three", "db-small")
	if got := quota(err); got != "instances" {
		t.Errorf("third database: limit %q, want instances", got)
	}
	if !strings.Contains(err.Error(), "instances allows 2, you have 2") {
		t.Errorf("message = %q", err)
	}

	// An operator override of one limit at a time shows the next.
	if _, err := mig.Exec(ctx, `UPDATE subscriptions SET max_instances = 3 WHERE account_id = $1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(account, actor, "three", "db-small"); quota(err) != "storage_gb" {
		t.Errorf("storage: %v", err)
	}
	if _, err := mig.Exec(ctx, `UPDATE subscriptions SET max_storage_gb = 100 WHERE account_id = $1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(account, actor, "three", "db-small"); quota(err) != "connections" {
		t.Errorf("connections: %v", err)
	}
	if _, err := mig.Exec(ctx, `UPDATE subscriptions SET max_connections = 80 WHERE account_id = $1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(account, actor, "three", "db-small"); err != nil {
		t.Errorf("within the raised limits: %v", err)
	}
}

// The acceptance of #44, third part: 20 parallel creates against a quota of
// 2 give exactly 2.
func TestCreateParallelAgainstQuota(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	pool := f.db.PoolAs(dbtest.RoleAPI)
	ctx := context.Background()

	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		created, quotas int
		other           []error
	)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := create(ctx, pool, instances.CreateRequest{AccountID: account, Name: fmt.Sprintf("db-%d", i),
				FlavorCode: "db-small", RequestedBy: actor})
			mu.Lock()
			defer mu.Unlock()
			var q *instances.QuotaError
			switch {
			case err == nil:
				created++
			case errors.As(err, &q) && q.Limit == "instances":
				quotas++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()
	if created != 2 || quotas != 18 || len(other) != 0 {
		t.Errorf("created %d, over quota %d, other %v; want 2 and 18", created, quotas, other)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE account_id = $1`, account).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("instances = %d, %v; want 2", rows, err)
	}
}

// Placement under concurrency (#45): accounts racing for the last slots of
// a cluster get exactly as many as it has.
func TestCreateParallelAgainstClusterCapacity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.ConnAs(dbtest.RoleMigrator).Exec(ctx, `UPDATE clusters SET capacity_databases = 3`); err != nil {
		t.Fatal(err)
	}
	type who struct{ account, actor string }
	var customers []who
	for i := range 8 {
		a, actor := f.customer(fmt.Sprintf("user%d@example.com", i), true)
		customers = append(customers, who{a, actor})
	}
	pool := f.db.PoolAs(dbtest.RoleAPI)
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		created, noRoom int
		other           []error
	)
	for _, c := range customers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := create(ctx, pool, instances.CreateRequest{AccountID: c.account, Name: "production",
				FlavorCode: "db-small", RequestedBy: c.actor})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, instances.ErrNoCapacity):
				noRoom++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()
	if created != 3 || noRoom != 5 || len(other) != 0 {
		t.Errorf("created %d, no capacity %d, other %v; want 3 and 5", created, noRoom, other)
	}
}

func TestCreatePlacesOnTheClusterWithMostFreeStorage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	big := cluster("pgc2", "node-01", "10.64.0.11")
	big.CapacityStorageGB = 100
	if _, err := instances.AddCluster(ctx, admin, big); err != nil {
		t.Fatal(err)
	}
	account, actor := f.customer("ana@example.com", true)
	c, err := f.create(account, actor, "one", "db-small")
	if err != nil {
		t.Fatal(err)
	}
	clusterName := func(id string) string {
		t.Helper()
		var name string
		if err := admin.QueryRow(ctx, `SELECT name FROM clusters WHERE id = $1`, id).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	if got := clusterName(c.Instance.ClusterID); got != "pgc2" {
		t.Errorf("placed on %s, want pgc2 (100 GB free against 60)", got)
	}

	// Draining pgc2 sends the next one to pgc1; draining both leaves no room.
	if _, err := instances.SetClusterStatus(ctx, admin, "pgc2", instances.StatusDraining); err != nil {
		t.Fatal(err)
	}
	c, err = f.create(account, actor, "two", "db-small")
	if err != nil || clusterName(c.Instance.ClusterID) != "pgc1" {
		t.Errorf("with pgc2 draining: %+v, %v", c.Instance, err)
	}
	if _, err := instances.SetClusterStatus(ctx, admin, "pgc1", instances.StatusDraining); err != nil {
		t.Fatal(err)
	}
	other, actor2 := f.customer("bob@example.com", true)
	if _, err := f.create(other, actor2, "one", "db-small"); !errors.Is(err, instances.ErrNoCapacity) {
		t.Errorf("everything draining: %v, want ErrNoCapacity", err)
	}
}

func TestCreateRefusesBadRequests(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	if _, err := f.create(account, actor, "production", "db-small"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(account, actor, "production", "db-small"); !errors.Is(err, instances.ErrInstanceNameTaken) {
		t.Errorf("same name: %v", err)
	}
	for _, name := range []string{"", "a", "Production", "1db", "db_one", strings.Repeat("a", 64)} {
		if _, err := f.create(account, actor, name, "db-small"); !errors.Is(err, instances.ErrInvalidName) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, err := f.create(account, actor, "other", "db-huge"); err == nil || !strings.Contains(err.Error(), "no such flavor") {
		t.Errorf("unknown flavor: %v", err)
	}
	// Another account may use the same name.
	other, actor2 := f.customer("bob@example.com", true)
	if _, err := f.create(other, actor2, "production", "db-small"); err != nil {
		t.Errorf("same name, other account: %v", err)
	}
}

func TestCreateIdempotencyKey(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	ctx := context.Background()
	api := f.db.ConnAs(dbtest.RoleAPI)
	req := instances.CreateRequest{AccountID: account, Name: "production", FlavorCode: "db-medium",
		RequestedBy: actor, IdempotencyKey: "k-1"}

	first, err := create(ctx, api, req)
	if err != nil || first.Replayed {
		t.Fatalf("first = %+v, %v", first, err)
	}
	// Fill the quota: a replay is still answered, because it succeeded.
	if _, err := f.create(account, actor, "other", "db-medium"); err != nil {
		t.Fatal(err)
	}
	again, err := create(ctx, api, req)
	if err != nil || !again.Replayed || again.Instance.ID != first.Instance.ID || again.Operation.ID != first.Operation.ID ||
		first.Password == "" || again.Password != "" {
		t.Errorf("replay = %+v, %v; want the first instance, replayed", again, err)
	}
	changed := req
	changed.Name = "staging"
	if _, err := create(ctx, api, changed); !errors.Is(err, instances.ErrIdempotencyMismatch) {
		t.Errorf("same key, other name: %v", err)
	}
	// Keys are per account.
	other, actor2 := f.customer("bob@example.com", true)
	if c, err := create(ctx, api, instances.CreateRequest{AccountID: other, Name: "production", FlavorCode: "db-medium",
		RequestedBy: actor2, IdempotencyKey: "k-1"}); err != nil || c.Replayed {
		t.Errorf("same key, other account = %+v, %v", c, err)
	}
	var rows int
	if err := api.QueryRow(ctx, `SELECT count(*) FROM instances`).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("instances = %d, %v; want 3", rows, err)
	}
}

func TestInstancePrivileges(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	if _, err := f.create(account, actor, "production", "db-small"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct{ role, sql string }{
		{dbtest.RoleAPI, `UPDATE instances SET storage_gb = 1000`},
		{dbtest.RoleAPI, `UPDATE instances SET status = 'deleted'`},
		{dbtest.RoleAPI, `DELETE FROM instances`},
		{dbtest.RoleAPI, `UPDATE operations SET status = 'succeeded'`},
		// The worker records observed state (#134), never the customer's intent.
		{dbtest.RoleWorker, `UPDATE instances SET desired_state = 'deleted'`},
		{dbtest.RoleAdmin, `SELECT 1 FROM operations`},
	} {
		_, err := f.db.ConnAs(tc.role).Exec(ctx, tc.sql)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %s = %v, want permission denied", tc.role, tc.sql, err)
		}
	}
	// Asking for a delete is all the API may change (#46).
	if _, err := f.db.ConnAs(dbtest.RoleAPI).Exec(ctx, `UPDATE instances SET desired_state = 'deleted'`); err != nil {
		t.Errorf("api sets desired_state: %v", err)
	}
}
