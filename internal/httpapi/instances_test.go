package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// send is do with request headers.
func (e *env) send(c *http.Client, method, path, body string, header map[string]string) (*http.Response, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			e.t.Fatalf("%s %s: body %q is not JSON", method, path, data)
		}
	}
	return resp, out
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func (e *env) auditCount(action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCreateListGetInstance(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")

	resp, sub := e.do(ana, "GET", "/api/subscription", "")
	limits := obj(obj(sub["subscription"])["limits"])
	if resp.StatusCode != http.StatusOK || sub["entitled"] != true || obj(obj(sub["subscription"])["plan"])["name"] != "Founder Beta" ||
		obj(limits["instances"])["allowed"] != 2.0 || obj(limits["instances"])["allocated"] != 0.0 {
		t.Fatalf("subscription before: %d %v", resp.StatusCode, sub)
	}

	resp, body := e.do(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`)
	in, op := obj(body["instance"]), obj(body["operation"])
	if resp.StatusCode != http.StatusAccepted || in["name"] != "production" || in["status"] != "provisioning" ||
		in["health"] != "unknown" || in["connection"] != nil || obj(in["allocation"])["storage_gb"] != 2.0 ||
		op["kind"] != "create" || op["status"] != "pending" || obj(in["operation"])["id"] != op["id"] {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if _, ok := body["password_shown"]; ok {
		t.Errorf("first create says password_shown: %v", body)
	}
	for _, internal := range []string{"cluster_id", "node_id", "infra_name", "cluster", "node"} {
		if _, ok := in[internal]; ok {
			t.Errorf("instance shows %q: %v", internal, in)
		}
	}
	if n := e.auditCount("instance.create"); n != 1 {
		t.Errorf("instance.create audited %d times", n)
	}

	id := in["id"].(string)
	resp, list := e.do(ana, "GET", "/api/instances", "")
	if items, _ := list["instances"].([]any); resp.StatusCode != http.StatusOK || len(items) != 1 || obj(items[0])["id"] != id {
		t.Errorf("list: %d %v", resp.StatusCode, list)
	}
	resp, got := e.do(ana, "GET", "/api/instances/"+id, "")
	if resp.StatusCode != http.StatusOK || got["name"] != "production" || obj(got["operation"])["kind"] != "create" {
		t.Errorf("get: %d %v", resp.StatusCode, got)
	}

	// The subscription counts what the quota check counts (#44).
	_, sub = e.do(ana, "GET", "/api/subscription", "")
	limits = obj(obj(sub["subscription"])["limits"])
	if obj(limits["instances"])["allocated"] != 1.0 || obj(limits["storage_gb"])["allocated"] != 2.0 ||
		obj(limits["connections"])["allocated"] != 20.0 {
		t.Errorf("subscription after one: %v", limits)
	}
	if _, ok := limits["memory_mb"]; ok {
		t.Errorf("a plan without memory shows a memory limit: %v", limits)
	}
}

func TestCreateInstanceRefusals(t *testing.T) {
	e := newEnv(t)
	e.withCluster()

	// Signed up, never subscribed: 403, and the subscription page says so.
	bob := e.signedUp("bob@example.com")
	if resp, body := e.do(bob, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no subscription: %d %v, want 403", resp.StatusCode, body)
	}
	if resp, sub := e.do(bob, "GET", "/api/subscription", ""); resp.StatusCode != http.StatusOK || sub["entitled"] != false || sub["subscription"] != nil {
		t.Errorf("no subscription page: %d %v", resp.StatusCode, sub)
	}

	ana, _ := e.customer("ana@example.com")
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"name":"Prod","flavor":"db-small"}`, http.StatusUnprocessableEntity},
		{`{"name":"production","flavor":"db-huge"}`, http.StatusUnprocessableEntity},
		{`{"name":"production","flavor":"db-small","cluster":"pgc1"}`, http.StatusBadRequest},
	} {
		if resp, body := e.do(ana, "POST", "/api/instances", tc.body); resp.StatusCode != tc.code {
			t.Errorf("%s: %d %v, want %d", tc.body, resp.StatusCode, body, tc.code)
		}
	}
	for _, name := range []string{"one", "two"} {
		if resp, body := e.do(ana, "POST", "/api/instances", `{"name":"`+name+`","flavor":"db-small"}`); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create %s: %d %v", name, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(ana, "POST", "/api/instances", `{"name":"one","flavor":"db-small"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("same name: %d %v", resp.StatusCode, body)
	}
	resp, body := e.do(ana, "POST", "/api/instances", `{"name":"three","flavor":"db-small"}`)
	if resp.StatusCode != http.StatusConflict || body["limit"] != "instances" {
		t.Errorf("over quota: %d %v, want 409 naming instances", resp.StatusCode, body)
	}
}

// A repeated create gets the same instance and operation, and no password
// (#46, #54).
func TestCreateInstanceIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	key := map[string]string{"Idempotency-Key": "portal-7f3a"}

	_, first := e.send(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`, key)
	resp, again := e.send(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`, key)
	if resp.StatusCode != http.StatusAccepted || again["password_shown"] != false ||
		obj(again["instance"])["id"] != obj(first["instance"])["id"] || obj(again["operation"])["id"] != obj(first["operation"])["id"] {
		t.Errorf("replay: %d %v\nfirst %v", resp.StatusCode, again, first)
	}
	if n := e.auditCount("instance.create"); n != 1 {
		t.Errorf("instance.create audited %d times, want once", n)
	}
	if resp, body := e.send(ana, "POST", "/api/instances", `{"name":"staging","flavor":"db-small"}`, key); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("same key, other request: %d %v", resp.StatusCode, body)
	}
	long := map[string]string{"Idempotency-Key": strings.Repeat("k", 256)}
	if resp, _ := e.send(ana, "POST", "/api/instances", `{"name":"staging","flavor":"db-small"}`, long); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("long key: %d", resp.StatusCode)
	}
}

// The acceptance of #46: another account's instance is a 404, for reading
// and for deleting.
func TestInstancesOfAnotherAccount(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	_, body := e.do(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`)
	id := obj(body["instance"])["id"].(string)

	bob, _ := e.customer("bob@example.com")
	if resp, _ := e.do(bob, "GET", "/api/instances/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get: %d", resp.StatusCode)
	}
	if resp, _ := e.do(bob, "DELETE", "/api/instances/"+id, `{"confirm":"production"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if resp, list := e.do(bob, "GET", "/api/instances", ""); resp.StatusCode != http.StatusOK || len(list["instances"].([]any)) != 0 {
		t.Errorf("list: %d %v", resp.StatusCode, list)
	}
	if resp, _ := e.do(bob, "GET", "/api/instances/not-a-uuid", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad id: %d", resp.StatusCode)
	}
}

// The acceptance of #46: delete with the wrong name → 422 and nothing
// queued; delete queues an operation, created but not run.
func TestDeleteInstance(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	_, body := e.do(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`)
	id := obj(body["instance"])["id"].(string)
	createOp := obj(body["operation"])["id"].(string)

	// The create is still pending: one operation at a time.
	if resp, body := e.do(ana, "DELETE", "/api/instances/"+id, `{"confirm":"production"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete while creating: %d %v, want 409", resp.StatusCode, body)
	}

	// What a worker writes when the create succeeds (M3).
	ctx := context.Background()
	mig := e.db.ConnAs(dbtest.RoleMigrator)
	if _, err := mig.Exec(ctx, `UPDATE operations SET status = 'succeeded', finished_at = now() WHERE id = $1`, createOp); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `UPDATE instances SET status = 'running', health = 'ok',
		endpoint_host = 'otc-0123456789ab.db.example.com', endpoint_port = 5432 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_, got := e.do(ana, "GET", "/api/instances/"+id, "")
	conn := obj(got["connection"])
	if got["status"] != "running" || got["health"] != "ok" || got["operation"] != nil ||
		conn["host"] != "otc-0123456789ab.db.example.com" || conn["port"] != 5432.0 || conn["tls"] != "required" ||
		!strings.HasPrefix(conn["database"].(string), "otc_") || conn["user"] != conn["database"] {
		t.Errorf("running: %v", got)
	}

	if resp, body := e.do(ana, "DELETE", "/api/instances/"+id, `{"confirm":"prod"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong name: %d %v, want 422", resp.StatusCode, body)
	}
	if resp, body := e.do(ana, "DELETE", "/api/instances/"+id, `{}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no name: %d %v, want 422", resp.StatusCode, body)
	}
	var queued int
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM operations WHERE instance_id = $1 AND kind = 'delete'`, id).Scan(&queued); err != nil || queued != 0 {
		t.Errorf("after refused deletes, %d delete operations queued, %v", queued, err)
	}

	resp, body := e.do(ana, "DELETE", "/api/instances/"+id, `{"confirm":"production"}`)
	in, op := obj(body["instance"]), obj(body["operation"])
	if resp.StatusCode != http.StatusAccepted || in["desired_state"] != "deleted" || in["status"] != "running" ||
		op["kind"] != "delete" || op["status"] != "pending" {
		t.Errorf("delete: %d %v", resp.StatusCode, body)
	}
	if resp, body := e.do(ana, "DELETE", "/api/instances/"+id, `{"confirm":"production"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete again: %d %v, want 409", resp.StatusCode, body)
	}
	if n := e.auditCount("instance.delete"); n != 1 {
		t.Errorf("instance.delete audited %d times", n)
	}
	// Until the worker deletes it, it still counts against the quota.
	_, sub := e.do(ana, "GET", "/api/subscription", "")
	if a := obj(obj(obj(sub["subscription"])["limits"])["instances"])["allocated"]; a != 1.0 {
		t.Errorf("allocated while deleting = %v, want 1", a)
	}
}
