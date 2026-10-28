package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
)

// withCluster gives the test database a node and a cluster to place on.
func (e *env) withCluster() {
	e.t.Helper()
	ctx := context.Background()
	admin := e.db.ConnAs(dbtest.RoleAdmin)
	if _, err := instances.AddNode(ctx, admin, instances.Node{Name: "node-01", CapacityMemoryMB: 8192, CapacityStorageGB: 200,
		TenantSubnet: netip.MustParsePrefix("10.64.0.0/22"), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := instances.AddCluster(ctx, admin, instances.Cluster{Name: "pgc1", Node: "node-01", PGMajor: 18, JailName: "otc-pgc1",
		PoolAddr: netip.MustParseAddr("10.64.0.10"), PoolPort: 6432, MemoryMB: 2048,
		CapacityStorageGB: 60, CapacityConnections: 300, CapacityDatabases: 12}); err != nil {
		e.t.Fatal(err)
	}
}

// customer is a signed-up, subscribed browser and its account id.
func (e *env) customer(email string) (*http.Client, string) {
	e.t.Helper()
	browser := e.signedUp(email)
	_, me := e.do(browser, "GET", "/api/me", "")
	account, _ := me["account_id"].(string)
	if _, err := billing.Subscribe(context.Background(), e.db.ConnAs(dbtest.RoleAdmin), account, "founder-beta"); err != nil {
		e.t.Fatal(err)
	}
	return browser, account
}

// createDatabase records a database for the account, as the API will, and
// returns its create operation.
func (e *env) createDatabase(account, name string) instances.Created {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var user string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE account_id = $1`, account).Scan(&user); err != nil {
		e.t.Fatal(err)
	}
	c, err := instances.Create(ctx, tx, instances.CreateRequest{AccountID: account, Name: name, FlavorCode: "db-small",
		RequestedBy: "user:" + user})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func TestOperations(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, account := e.customer("ana@example.com")
	c := e.createDatabase(account, "production")

	resp, op := e.do(ana, "GET", "/api/operations/"+c.Operation.ID, "")
	if resp.StatusCode != http.StatusOK || op["kind"] != "create" || op["status"] != "pending" ||
		op["instance_id"] != c.Instance.ID || op["requested_by"] != "you" || op["finished_at"] != nil ||
		!strings.HasSuffix(op["created_at"].(string), "Z") {
		t.Errorf("operation: %d %v", resp.StatusCode, op)
	}
	for _, path := range []string{"/api/operations", "/api/instances/" + c.Instance.ID + "/operations"} {
		resp, body := e.do(ana, "GET", path, "")
		ops, _ := body["operations"].([]any)
		if resp.StatusCode != http.StatusOK || len(ops) != 1 || ops[0].(map[string]any)["id"] != c.Operation.ID {
			t.Errorf("%s: %d %v", path, resp.StatusCode, body)
		}
	}

	resp, _ = e.do(e.client(), "GET", "/api/operations/"+c.Operation.ID, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d", resp.StatusCode)
	}
}

// The acceptance of #77: another account's operation is a 404, the same as
// one that doesn't exist.
func TestOperationsOfAnotherAccount(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	_, anaAccount := e.customer("ana@example.com")
	c := e.createDatabase(anaAccount, "production")
	bob, _ := e.customer("bob@example.com")

	for _, path := range []string{
		"/api/operations/" + c.Operation.ID,
		"/api/instances/" + c.Instance.ID + "/operations",
		"/api/operations/0190a000-0000-7000-8000-000000000000",
		"/api/operations/not-a-uuid",
		"/api/instances/not-a-uuid/operations",
	} {
		if resp, body := e.do(bob, "GET", path, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d %v, want 404", path, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(bob, "GET", "/api/operations", ""); resp.StatusCode != http.StatusOK ||
		len(body["operations"].([]any)) != 0 {
		t.Errorf("bob's list: %d %v, want empty", resp.StatusCode, body)
	}
}

// The acceptance of #77: a failed operation whose last_error names a host,
// a cluster and SQL shows its code and message, and nothing else.
func TestFailedOperationShowsOnlyItsReason(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, account := e.customer("ana@example.com")
	c := e.createDatabase(account, "production")

	// What a worker would write after its retries ran out (M3).
	const lastError = `connect to otc-pgc1 at 10.64.0.10:6432: ERROR: permission denied for CREATE ROLE "otc_0123456789ab" (SQLSTATE 42501)`
	if _, err := e.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), `
		UPDATE operations SET status = 'failed', reason = 'create_failed', step = 'role_created', attempts = 5,
			locked_by = 'node-01/worker', last_error = $2, finished_at = now()
		WHERE id = $1`, c.Operation.ID, lastError); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/operations/" + c.Operation.ID, "/api/operations"} {
		req, err := http.NewRequest("GET", e.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := ana.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := string(data)
		if !strings.Contains(body, `"reason":"create_failed"`) || !strings.Contains(body, "could not be created") {
			t.Errorf("%s: %s, want the code and its message", path, body)
		}
		for _, leak := range []string{"10.64.0.10", "otc-pgc1", "otc_0123456789ab", "SQLSTATE", "permission denied",
			"role_created", "node-01", "last_error", "attempts", "locked_by", "step"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaks %q: %s", path, leak, body)
			}
		}
	}
}
