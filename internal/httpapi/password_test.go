package httpapi_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// The acceptance of #101 at the API: a reset queues a reset_password with
// a new verifier and shows the password once; a replay shows none; it
// takes a running or stopped database only, one operation at a time, and
// never another account's.
func TestResetInstancePassword(t *testing.T) {
	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	bob, _ := e.customer("bob@example.com")
	_, body := e.do(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`)
	id := obj(body["instance"])["id"].(string)
	path := "/api/instances/" + id + "/reset-password"

	if resp, body := e.do(ana, "POST", path, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("reset while provisioning: %d %v, want 409", resp.StatusCode, body)
	}
	ctx := context.Background()
	mig := e.db.ConnAs(dbtest.RoleMigrator)
	finish := func(op, status string) {
		t.Helper()
		if _, err := mig.Exec(ctx, `UPDATE operations SET status = 'succeeded', finished_at = now(), payload = '{}' WHERE id = $1`, op); err != nil {
			t.Fatal(err)
		}
		if _, err := mig.Exec(ctx, `UPDATE instances SET status = $2 WHERE id = $1`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	finish(obj(body["operation"])["id"].(string), "running")

	if resp, _ := e.do(bob, "POST", path, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob resets ana's password: %d, want 404", resp.StatusCode)
	}

	key := map[string]string{"Idempotency-Key": "reset-1"}
	resp, first := e.send(ana, "POST", path, "", key)
	password, _ := first["password"].(string)
	op := obj(first["operation"])
	if resp.StatusCode != http.StatusAccepted || !regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(password) ||
		op["kind"] != "reset_password" || op["status"] != "pending" {
		t.Fatalf("reset: %d %v", resp.StatusCode, first)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var verifier string
	if err := mig.QueryRow(ctx, `SELECT payload->>'verifier' FROM operations WHERE id = $1`, op["id"]).Scan(&verifier); err != nil ||
		!strings.HasPrefix(verifier, "SCRAM-SHA-256$") || strings.Contains(verifier, password) {
		t.Errorf("the operation's verifier %q, %v", verifier, err)
	}
	if strings.Contains(e.logs.String(), password) {
		t.Error("the password is in the logs")
	}

	resp, again := e.send(ana, "POST", path, "", key)
	if _, ok := again["password"]; ok || resp.StatusCode != http.StatusAccepted || again["password_shown"] != false ||
		obj(again["operation"])["id"] != op["id"] {
		t.Errorf("replay: %d %v", resp.StatusCode, again)
	}
	create := map[string]string{"Idempotency-Key": "reset-1"}
	if resp, body := e.send(ana, "POST", "/api/instances", `{"name":"staging","flavor":"db-small"}`, create); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("the reset's key on a create: %d %v, want 422", resp.StatusCode, body)
	}
	if resp, body := e.do(ana, "POST", path, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("reset while one is pending: %d %v, want 409", resp.StatusCode, body)
	}
	if n := e.auditCount("instance.reset_password"); n != 1 {
		t.Errorf("instance.reset_password audited %d times, want once", n)
	}
	finish(op["id"].(string), "stopped")

	if resp, body := e.do(ana, "POST", path, ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("reset a stopped one: %d %v", resp.StatusCode, body)
	}
}
