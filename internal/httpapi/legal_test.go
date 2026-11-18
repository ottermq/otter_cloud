package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/httpapi"
	"github.com/ottermq/otter_cloud/internal/legal"
)

func legalPages(t *testing.T) *legal.Pages {
	t.Helper()
	p, err := legal.Render(legal.Fields{Controller: "Ana Souza", Contact: "contato@example.com",
		EdgeCountry: "Brasil", Published: "2026-11-20"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLegalPages(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{Legal: legalPages(t)})
	anon := e.client()
	for _, name := range []string{"termos", "privacidade"} {
		resp, body := e.do(anon, "GET", "/api/legal/"+name, "")
		html, _ := body["html"].(string)
		if resp.StatusCode != http.StatusOK || body["version"] != "2026-11-20" || body["name"] != name ||
			body["title"] == "" || !strings.Contains(html, "Ana Souza") {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body["title"])
		}
	}
	if resp, _ := e.do(anon, "GET", "/api/legal/other", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown page: %d, want 404", resp.StatusCode)
	}

	// Unpublished: 404, and sign-up asks for nothing.
	e = newEnv(t)
	if resp, body := e.do(e.client(), "GET", "/api/legal/termos", ""); resp.StatusCode != http.StatusNotFound || body["error"] != "not published" {
		t.Errorf("unpublished: %d %v", resp.StatusCode, body)
	}
	resp, body := e.do(e.client(), "POST", "/api/signup",
		`{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("signup with nothing published: %d %v", resp.StatusCode, body)
	}
}

func TestSignUpAcceptsTheTerms(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{Legal: legalPages(t)})
	token := e.invite("ana@example.com")
	for _, accepted := range []string{"", "2026-01-01"} {
		resp, body := e.do(e.client(), "POST", "/api/signup",
			`{"token":"`+token+`","password":"`+goodPassword+`","accepted_terms":"`+accepted+`"}`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body["error"].(string), "terms") {
			t.Errorf("accepted %q: %d %v, want 400", accepted, resp.StatusCode, body)
		}
	}
	// The refusal didn't use the invite.
	resp, body := e.do(e.client(), "POST", "/api/signup",
		`{"token":"`+token+`","password":"`+goodPassword+`","accepted_terms":"2026-11-20"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup: %d %v", resp.StatusCode, body)
	}
	var version string
	if err := e.db.ConnAs(dbtest.RoleAPI).QueryRow(context.Background(),
		`SELECT details->>'accepted_terms' FROM audit_events WHERE action = 'auth.signup' AND outcome = 'ok'`).
		Scan(&version); err != nil || version != "2026-11-20" {
		t.Errorf("the signup event records %q (%v), want the version accepted", version, err)
	}
}
