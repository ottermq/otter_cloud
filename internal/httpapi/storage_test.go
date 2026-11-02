package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// #99 over HTTP: the instance shows its storage, and a blocked database
// shows why and can't be started or restarted; stopping it is fine.
func TestInstanceStorage(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	_, out := e.do(ana, "POST", "/api/instances", `{"name":"big","flavor":"db-small"}`)
	in := obj(out["instance"])
	st := obj(in["storage"])
	if st["state"] != "ok" || st["used_bytes"] != nil || st["allocated_bytes"] != float64(2<<30) || st["measured_at"] != nil {
		t.Errorf("storage before a measurement: %v", st)
	}
	id, _ := in["id"].(string)
	path := "/api/instances/" + id
	for _, sql := range []string{
		`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE instance_id = $1`,
		`UPDATE instances SET status = 'running', storage_state = 'blocked', storage_used_bytes = 2684354560,
			storage_measured_at = now() WHERE id = $1`,
	} {
		if _, err := e.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), sql, id); err != nil {
			t.Fatal(err)
		}
	}
	_, v := e.do(ana, "GET", path, "")
	st = obj(v["storage"])
	if st["state"] != "blocked" || st["used_bytes"] != float64(2684354560) || st["measured_at"] == nil ||
		v["reason"] != "storage_blocked" || v["message"] == nil {
		t.Errorf("a blocked database: %v", v)
	}
	for _, verb := range []string{"start", "restart"} {
		if resp, out := e.do(ana, "POST", path+"/"+verb, ""); resp.StatusCode != http.StatusConflict {
			t.Errorf("%s a blocked database: %d %v, want 409", verb, resp.StatusCode, out)
		}
	}
	if resp, out := e.do(ana, "POST", path+"/stop", ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("stop a blocked database: %d %v", resp.StatusCode, out)
	}
}
