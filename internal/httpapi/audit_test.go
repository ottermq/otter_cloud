package httpapi_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/ottermq/otter_cloud/internal/httpapi"
)

type event struct {
	actor, action, outcome, reason string
	hasAccount                     bool
}

func (e *env) events() []event {
	e.t.Helper()
	rows, err := e.db.ConnAs("otter_api").Query(e.t.Context(),
		"SELECT actor, action, outcome, coalesce(details->>'reason', ''), account_id IS NOT NULL FROM audit_events ORDER BY at, id")
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []event
	for rows.Next() {
		var ev event
		if err := rows.Scan(&ev.actor, &ev.action, &ev.outcome, &ev.reason, &ev.hasAccount); err != nil {
			e.t.Fatal(err)
		}
		if ev.actor != "anonymous" {
			ev.actor = ev.actor[:len("user:")] // the id varies
		}
		out = append(out, ev)
	}
	return out
}

func TestAuthEventsAreAudited(t *testing.T) {
	public, _ := url.Parse("https://cloud.example.com")
	e := newEnvWith(t, httpapi.Options{PublicURL: public})
	c := e.client()

	e.post(c, "/api/signup", `{"token":"bogus","password":"`+goodPassword+`"}`, nil)
	e.post(c, "/api/signup", `{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`, nil)
	e.post(c, "/api/logout", "", map[string]string{"Origin": "https://evil.example"})
	e.post(c, "/api/logout", "", nil)
	e.post(c, "/api/logout", "", nil) // no session: nothing to record
	e.post(c, "/api/login", `{"email":"ana@example.com","password":"wrong password!"}`, nil)
	if resp := e.post(c, "/api/login", `{"email":"ana@example.com","password":"`+goodPassword+`"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	want := []event{
		{"anonymous", "auth.signup", "denied", "invite_invalid", false},
		{"user:", "auth.signup", "ok", "", true},
		{"anonymous", "auth.cross_origin_refused", "denied", "", false},
		{"user:", "auth.logout", "ok", "", true},
		{"anonymous", "auth.login", "denied", "bad_credentials", false},
		{"user:", "auth.login", "ok", "", true},
	}
	got := e.events()
	if len(got) != len(want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %v, want %v", i, got[i], want[i])
		}
	}
}
