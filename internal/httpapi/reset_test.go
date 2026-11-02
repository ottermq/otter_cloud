package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

// The acceptance of #160 over HTTP: a link sets the password once, logs
// the request in, refuses every older cookie, and lifts a lockout the
// forgotten password caused.
func TestPasswordReset(t *testing.T) {
	e := newEnv(t)
	ana := e.signedUp("ana@example.com")
	for range 5 {
		e.do(e.client(), "POST", "/api/login", `{"email":"ana@example.com","password":"not the password"}`)
	}
	if resp, _ := e.do(e.client(), "POST", "/api/login", `{"email":"ana@example.com","password":"`+goodPassword+`"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("login after 5 failures: %d, want 429", resp.StatusCode)
	}
	_, token, err := identity.CreateReset(context.Background(), e.db.ConnAs(dbtest.RoleAdmin), "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	browser := e.client()
	body := `{"token":"` + token + `","password":"` + newPassword + `"}`
	resp, out := e.do(browser, "POST", "/api/password-reset", body)
	if resp.StatusCode != http.StatusOK || out["email"] != "ana@example.com" || findCookie(resp) == nil {
		t.Fatalf("reset: %d %v", resp.StatusCode, out)
	}
	if resp, _ := e.do(browser, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("me after the reset: %d", resp.StatusCode)
	}
	if resp, _ := e.do(ana, "GET", "/api/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an older session after the reset: %d, want 401", resp.StatusCode)
	}
	e.login("ana@example.com", newPassword)

	if resp, out := e.do(e.client(), "POST", "/api/password-reset", body); resp.StatusCode != http.StatusBadRequest || out["error"] == nil {
		t.Errorf("reused link: %d %v, want 400", resp.StatusCode, out)
	}
	if n := e.auditCount("auth.password_reset"); n != 2 {
		t.Errorf("%d auth.password_reset events, want 2 (ok, then denied)", n)
	}
}

func TestPasswordResetFailures(t *testing.T) {
	e := newEnv(t)
	e.signedUp("ana@example.com")
	_, token, err := identity.CreateReset(context.Background(), e.db.ConnAs(dbtest.RoleAdmin), "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"token":"nope","password":"` + newPassword + `"}`,
		`{"token":"` + token + `","password":"short"}`,
		`{"token":"` + token + `","password":"qwertyuiop123"}`,
	} {
		if resp, out := e.do(e.client(), "POST", "/api/password-reset", body); resp.StatusCode != http.StatusBadRequest || out["error"] == nil {
			t.Errorf("reset %s: %d %v, want 400", body, resp.StatusCode, out)
		}
	}
	// Bad links count against the address, as bad invites do.
	for range 20 {
		e.do(e.client(), "POST", "/api/password-reset", `{"token":"nope","password":"`+newPassword+`"}`)
	}
	if resp, _ := e.do(e.client(), "POST", "/api/password-reset", `{"token":"`+token+`","password":"`+newPassword+`"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after 20 bad links: %d, want 429", resp.StatusCode)
	}
}
