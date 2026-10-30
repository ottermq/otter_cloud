package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// The acceptance of #95 at the API: stop, start and restart queue their
// operation, one at a time; each starts only from the statuses it can, and
// a stopped database still counts against the quota.
func TestStopStartRestartInstance(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	bob, _ := e.customer("bob@example.com")
	_, body := e.do(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`)
	id := obj(body["instance"])["id"].(string)
	path := "/api/instances/" + id

	if resp, body := e.do(ana, "POST", path+"/stop", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("stop while provisioning: %d %v, want 409", resp.StatusCode, body)
	}
	ctx := context.Background()
	mig := e.db.ConnAs(dbtest.RoleMigrator)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := mig.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// What the worker records: the operation done, the instance as asked.
	finish := func(op, status string) {
		t.Helper()
		exec(`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE id = $1`, op)
		exec(`UPDATE instances SET status = $2 WHERE id = $1`, id, status)
	}
	finish(obj(body["operation"])["id"].(string), "running")

	for _, tc := range []struct{ who, verb string }{{"bob", "stop"}, {"bob", "start"}, {"bob", "restart"}} {
		if resp, _ := e.do(bob, "POST", path+"/"+tc.verb, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %ss ana's database: %d, want 404", tc.who, tc.verb, resp.StatusCode)
		}
	}
	if resp, body := e.do(ana, "POST", path+"/start", ""); resp.StatusCode != http.StatusConflict || body["error"] != "this database is already running" {
		t.Errorf("start a running one: %d %v", resp.StatusCode, body)
	}

	resp, body := e.do(ana, "POST", path+"/stop", "")
	in, op := obj(body["instance"]), obj(body["operation"])
	if resp.StatusCode != http.StatusAccepted || in["desired_state"] != "stopped" || in["status"] != "running" ||
		op["kind"] != "stop" || op["status"] != "pending" {
		t.Fatalf("stop: %d %v", resp.StatusCode, body)
	}
	for _, verb := range []string{"stop", "start", "restart"} {
		if resp, body := e.do(ana, "POST", path+"/"+verb, ""); resp.StatusCode != http.StatusConflict {
			t.Errorf("%s while the stop is pending: %d %v, want 409", verb, resp.StatusCode, body)
		}
	}
	finish(op["id"].(string), "stopped")

	_, got := e.do(ana, "GET", path, "")
	if got["status"] != "stopped" || got["connection"] != nil {
		t.Errorf("stopped: %v", got)
	}
	_, sub := e.do(ana, "GET", "/api/subscription", "")
	if a := obj(obj(obj(sub["subscription"])["limits"])["instances"])["allocated"]; a != 1.0 {
		t.Errorf("allocated while stopped = %v, want 1", a)
	}
	if resp, body := e.do(ana, "POST", path+"/restart", ""); resp.StatusCode != http.StatusConflict ||
		body["error"] != "this database is stopped; start it instead" {
		t.Errorf("restart a stopped one: %d %v", resp.StatusCode, body)
	}
	if resp, body := e.do(ana, "POST", path+"/stop", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("stop a stopped one: %d %v", resp.StatusCode, body)
	}

	resp, body = e.do(ana, "POST", path+"/start", "")
	if in := obj(body["instance"]); resp.StatusCode != http.StatusAccepted || in["desired_state"] != "running" || obj(body["operation"])["kind"] != "start" {
		t.Fatalf("start: %d %v", resp.StatusCode, body)
	}
	finish(obj(body["operation"])["id"].(string), "running")

	resp, body = e.do(ana, "POST", path+"/restart", "")
	if in := obj(body["instance"]); resp.StatusCode != http.StatusAccepted || in["desired_state"] != "running" || obj(body["operation"])["kind"] != "restart" {
		t.Fatalf("restart: %d %v", resp.StatusCode, body)
	}

	for action, want := range map[string]int{"instance.stop": 1, "instance.start": 1, "instance.restart": 1} {
		if n := e.auditCount(action); n != want {
			t.Errorf("%s audited %d times, want %d", action, n, want)
		}
	}
	// A deleted one takes none of them.
	exec(`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE instance_id = $1 AND status = 'pending'`, id)
	exec(`UPDATE instances SET desired_state = 'deleted' WHERE id = $1`, id)
	if resp, body := e.do(ana, "POST", path+"/stop", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("stop a deleted one: %d %v", resp.StatusCode, body)
	}
}
