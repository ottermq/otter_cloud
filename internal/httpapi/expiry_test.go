package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// #100 over HTTP: an expiry and ephemeral at create, shown on the
// instance; PATCH moves or clears the expiry, audited; an expired database
// can't be started until it does.
func TestInstanceExpiry(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")

	soon := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	if resp, out := e.do(ana, "POST", "/api/instances", `{"name":"scratch","flavor":"db-small","expires_at":"`+soon+`"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expiry in 10 minutes: %d %v, want 422", resp.StatusCode, out)
	}
	at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	resp, out := e.do(ana, "POST", "/api/instances",
		`{"name":"scratch","flavor":"db-small","expires_at":"`+at.Format(time.RFC3339)+`","ephemeral":true}`)
	in := obj(out["instance"])
	if resp.StatusCode != http.StatusAccepted || in["expires_at"] != at.Format(time.RFC3339) || in["ephemeral"] != true {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	id, _ := in["id"].(string)
	path := "/api/instances/" + id

	if resp, out := e.do(ana, "PATCH", path, `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PATCH with nothing: %d %v, want 400", resp.StatusCode, out)
	}
	later := at.Add(24 * time.Hour)
	if resp, out := e.do(ana, "PATCH", path, `{"expires_at":"`+later.Format(time.RFC3339)+`"}`); resp.StatusCode != http.StatusOK || out["expires_at"] != later.Format(time.RFC3339) {
		t.Errorf("move the expiry: %d %v", resp.StatusCode, out)
	}
	if resp, out := e.do(ana, "PATCH", path, `{"expires_at":null}`); resp.StatusCode != http.StatusOK || out["expires_at"] != nil {
		t.Errorf("clear the expiry: %d %v", resp.StatusCode, out)
	}
	if n := e.auditCount("instance.set_expiry"); n != 2 {
		t.Errorf("%d instance.set_expiry events, want 2", n)
	}
	bob, _ := e.customer("bob@example.com")
	if resp, _ := e.do(bob, "PATCH", path, `{"expires_at":null}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another account's database: %d, want 404", resp.StatusCode)
	}

	// Stopped past its expiry, as the policy leaves it: the reason says so,
	// and start is refused.
	for _, sql := range []string{
		`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE instance_id = $1`,
		`UPDATE instances SET status = 'stopped', desired_state = 'stopped', expires_at = now() - interval '1 minute' WHERE id = $1`,
	} {
		if _, err := e.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), sql, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, out := e.do(ana, "GET", path, ""); out["reason"] != "expired" || out["message"] == nil {
		t.Errorf("an expired database: %v", out)
	}
	if resp, out := e.do(ana, "POST", path+"/start", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("start an expired database: %d %v, want 409", resp.StatusCode, out)
	}
	if resp, _ := e.do(ana, "PATCH", path, `{"expires_at":null}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear the expiry: %d", resp.StatusCode)
	}
	if resp, out := e.do(ana, "POST", path+"/start", ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("start once the expiry is cleared: %d %v", resp.StatusCode, out)
	}
}
