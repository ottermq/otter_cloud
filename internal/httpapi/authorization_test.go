package httpapi_test

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// apiRoute is how one endpoint is authorized: public, or for a session,
// and, when its path names something, only the account that owns it.
type apiRoute struct {
	public bool
	// owned names what {id} is, of ana's: "instance", "operation" or
	// "session". Another account gets 404 for it, the same as for an id
	// that doesn't exist.
	owned string
	body  string
}

// apiRoutes is every endpoint under /api. A new one fails
// TestEveryEndpointIsAuthorized until it is added here.
var apiRoutes = map[string]apiRoute{
	"POST /api/signup":         {public: true},
	"POST /api/login":          {public: true},
	"POST /api/password-reset": {public: true},
	"POST /api/logout":         {public: true}, // ends the session it is sent with, if any
	"GET /api/legal/{name}":    {public: true}, // the terms and privacy notice (#125)

	"GET /api/me":                  {},
	"POST /api/account/password":   {body: `{}`},
	"GET /api/account/sessions":    {},
	"DELETE /api/account/sessions": {},
	"GET /api/catalog":             {},
	"GET /api/operations":          {},
	"GET /api/subscription":        {},
	"GET /api/instances":           {},
	"POST /api/instances":          {body: `{"name":"other","flavor":"db-small"}`},

	"DELETE /api/account/sessions/{id}":       {owned: "session"},
	"GET /api/operations/{id}":                {owned: "operation"},
	"GET /api/instances/{id}":                 {owned: "instance"},
	"PATCH /api/instances/{id}":               {owned: "instance", body: `{"expires_at":null}`},
	"DELETE /api/instances/{id}":              {owned: "instance", body: `{"confirm":"production"}`},
	"POST /api/instances/{id}/stop":           {owned: "instance"},
	"POST /api/instances/{id}/start":          {owned: "instance"},
	"POST /api/instances/{id}/restart":        {owned: "instance"},
	"POST /api/instances/{id}/reset-password": {owned: "instance"},
	"GET /api/instances/{id}/operations":      {owned: "instance"},
}

// registeredRoutes reads the API's routes from server.go, where they are
// all registered.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`\.Handle(?:Func)?\("((?:GET|POST|PUT|PATCH|DELETE) /api/[^"]*)"`).FindAllSubmatch(src, -1) {
		routes = append(routes, string(m[1]))
	}
	if len(routes) == 0 {
		t.Fatal("no /api routes found in server.go")
	}
	return routes
}

// The acceptance of security.md §9's "authorization tests cover every
// endpoint": each one but the public five refuses a request without a
// session, and each that names something of an account's is a 404 to
// another account, whose request changes nothing.
func TestEveryEndpointIsAuthorized(t *testing.T) {
	registered := registeredRoutes(t)
	for _, r := range registered {
		if _, ok := apiRoutes[r]; !ok {
			t.Errorf("%s is not in apiRoutes: say how it is authorized", r)
		}
	}
	for r := range apiRoutes {
		if !slices.Contains(registered, r) {
			t.Errorf("%s is in apiRoutes but not registered", r)
		}
	}

	e := newEnv(t)
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	bob, _ := e.customer("bob@example.com")
	_, created := e.do(ana, "POST", "/api/instances", `{"name":"production","flavor":"db-small"}`)
	_, sessions := e.do(ana, "GET", "/api/account/sessions", "")
	list, _ := sessions["sessions"].([]any)
	if len(list) != 1 {
		t.Fatalf("ana's sessions: %v", sessions)
	}
	ids := map[string]string{
		"instance":  obj(created["instance"])["id"].(string),
		"operation": obj(created["operation"])["id"].(string),
		"session":   obj(list[0])["id"].(string),
	}

	anonymous := e.client()
	for route, a := range apiRoutes {
		method, path, _ := strings.Cut(route, " ")
		if a.owned != "" {
			path = strings.Replace(path, "{id}", ids[a.owned], 1)
		}
		if !a.public {
			if resp, body := e.do(anonymous, method, path, a.body); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s without a session: %d %v, want 401", route, resp.StatusCode, body)
			}
		}
		if a.owned != "" {
			if resp, body := e.do(bob, method, path, a.body); resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s by another account: %d %v, want 404", route, resp.StatusCode, body)
			}
		}
	}

	// Nothing of ana's changed.
	if resp, view := e.do(ana, "GET", "/api/instances/"+ids["instance"], ""); resp.StatusCode != http.StatusOK ||
		view["desired_state"] != "running" || view["expires_at"] != nil {
		t.Errorf("ana's database after bob's requests: %d %v", resp.StatusCode, view)
	}
	if resp, _ := e.do(ana, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("ana's session after bob's requests: %d", resp.StatusCode)
	}
}
