package httpapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// login opens another session for email, as a second browser would.
func (e *env) login(email, password string) *http.Client {
	e.t.Helper()
	browser := e.client()
	if resp, body := e.do(browser, "POST", "/api/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)); resp.StatusCode != http.StatusOK {
		e.t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	return browser
}

// replay is a browser holding a copy of c's session cookie as it is now.
func (e *env) replay(c *http.Client) *http.Client {
	e.t.Helper()
	u, _ := url.Parse(e.server.URL)
	copied := e.client()
	copied.Jar.SetCookies(u, c.Jar.Cookies(u))
	return copied
}

const newPassword = "a different horse battery staple"

// The acceptance of #78, password: a wrong current password is refused
// and counts against the login limit; a change ends every session, the
// changing one's old cookie included, and gives the request a new one.
func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	ana := e.signedUp("ana@example.com")
	laptop := e.login("ana@example.com", goodPassword)
	before := e.replay(ana)

	path := "/api/account/password"
	if resp, body := e.do(ana, "POST", path, `{"current_password":"wrong password here","new_password":"`+newPassword+`"}`); resp.StatusCode != http.StatusForbidden ||
		body["error"] != "the current password is incorrect" {
		t.Errorf("wrong current: %d %v", resp.StatusCode, body)
	}
	if resp, body := e.do(ana, "POST", path, `{"current_password":"`+goodPassword+`","new_password":"short"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("too short: %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do(laptop, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Fatal("a refused change ended a session")
	}

	resp, body := e.do(ana, "POST", path, `{"current_password":"`+goodPassword+`","new_password":"`+newPassword+`"}`)
	if resp.StatusCode != http.StatusOK || body["email"] != "ana@example.com" {
		t.Fatalf("change: %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do(ana, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the changing browser's new session: %d", resp.StatusCode)
	}
	for name, c := range map[string]*http.Client{"the old cookie": before, "another session": laptop} {
		if resp, _ := e.do(c, "GET", "/api/me", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s after the change: %d, want 401", name, resp.StatusCode)
		}
	}
	if resp, _ := e.do(e.client(), "POST", "/api/login", `{"email":"ana@example.com","password":"`+goodPassword+`"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the old password: %d", resp.StatusCode)
	}
	e.login("ana@example.com", newPassword)
	if n := e.auditCount("user.password_change"); n != 2 {
		t.Errorf("user.password_change audited %d times, want the refusal and the change", n)
	}
}

// Wrong current passwords count as failed logins: after five, even the
// right one is refused for a while, and so is a login.
func TestChangePasswordRateLimited(t *testing.T) {
	e := newEnv(t)
	ana := e.signedUp("ana@example.com")
	for range 5 {
		e.do(ana, "POST", "/api/account/password", `{"current_password":"wrong password here","new_password":"`+newPassword+`"}`)
	}
	if resp, body := e.do(ana, "POST", "/api/account/password", `{"current_password":"`+goodPassword+`","new_password":"`+newPassword+`"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("sixth try: %d %v, want 429", resp.StatusCode, body)
	}
	if resp, _ := e.do(e.client(), "POST", "/api/login", `{"email":"ana@example.com","password":"`+goodPassword+`"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("login after: %d, want 429", resp.StatusCode)
	}
}

// The acceptance of #78, sessions: a user lists their live sessions, the
// current one marked; ends one, or all others; never sees or ends another
// user's (404).
func TestSessions(t *testing.T) {
	e := newEnv(t)
	ana := e.signedUp("ana@example.com")
	laptop := e.login("ana@example.com", goodPassword)
	phone := e.login("ana@example.com", goodPassword)
	bob := e.signedUp("bob@example.com")

	_, body := e.do(ana, "GET", "/api/account/sessions", "")
	list, _ := body["sessions"].([]any)
	if len(list) != 3 {
		t.Fatalf("sessions: %v", body)
	}
	var current, other string
	for _, s := range list {
		if obj(s)["current"] == true {
			current = obj(s)["id"].(string)
		} else if other == "" {
			other = obj(s)["id"].(string)
		}
	}
	if current == "" || obj(list[0])["user_agent"] == nil {
		t.Fatalf("sessions: %v", list)
	}
	_, bobs := e.do(bob, "GET", "/api/account/sessions", "")
	if l, _ := bobs["sessions"].([]any); len(l) != 1 {
		t.Errorf("bob's sessions: %v", bobs)
	}
	if resp, _ := e.do(bob, "DELETE", "/api/account/sessions/"+other, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob ends ana's session: %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.do(ana, "DELETE", "/api/account/sessions/not-a-uuid", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a malformed id: %d, want 404", resp.StatusCode)
	}

	if resp, _ := e.do(ana, "DELETE", "/api/account/sessions/"+other, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end one: %d", resp.StatusCode)
	}
	if resp, _ := e.do(ana, "DELETE", "/api/account/sessions/"+other, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("end it again: %d, want 404", resp.StatusCode)
	}
	alive := 0
	for _, c := range []*http.Client{laptop, phone} {
		if resp, _ := e.do(c, "GET", "/api/me", ""); resp.StatusCode == http.StatusOK {
			alive++
		}
	}
	if alive != 1 {
		t.Errorf("%d of the two other sessions alive after ending one, want 1", alive)
	}

	resp, body := e.do(ana, "DELETE", "/api/account/sessions", "")
	if resp.StatusCode != http.StatusOK || body["ended"] != 1.0 {
		t.Errorf("end the others: %d %v", resp.StatusCode, body)
	}
	for _, c := range []*http.Client{laptop, phone} {
		if resp, _ := e.do(c, "GET", "/api/me", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("another session after ending the others: %d", resp.StatusCode)
		}
	}
	if resp, _ := e.do(ana, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Error("ending the others ended this one")
	}
	if resp, _ := e.do(bob, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Error("ending ana's others ended bob's")
	}

	// Ending the current one is a logout.
	if resp, _ := e.do(ana, "DELETE", "/api/account/sessions/"+current, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end the current one: %d", resp.StatusCode)
	}
	if resp, _ := e.do(ana, "GET", "/api/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after ending the current one: %d", resp.StatusCode)
	}
	if n := e.auditCount("session.revoke"); n != 3 {
		t.Errorf("session.revoke audited %d times, want 3", n)
	}
}
