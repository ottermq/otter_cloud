package httpapi_test

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/httpapi"
)

func (e *env) post(c *http.Client, path, body string, header map[string]string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest("POST", e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestCrossOriginPostRefused(t *testing.T) {
	public, _ := url.Parse("https://cloud.example.com")
	e := newEnvWith(t, httpapi.Options{PublicURL: public})
	browser := e.client()
	e.post(browser, "/api/signup", `{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`, nil)

	self := strings.TrimPrefix(e.server.URL, "https://")
	for _, tc := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-site sibling", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.cloud.example.com"}, http.StatusForbidden},
		{"foreign Origin, older browser", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://" + self}, http.StatusNoContent},
		{"the public origin behind a proxy", map[string]string{"Origin": "https://cloud.example.com"}, http.StatusNoContent},
		{"not a browser", nil, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Logout is a state change any logged-in browser can be tricked into.
			if resp := e.post(browser, "/api/logout", "", tc.header); resp.StatusCode != tc.want {
				t.Errorf("POST /api/logout: %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	// GET stays allowed cross-origin: it changes nothing.
	req, _ := http.NewRequest("GET", e.server.URL+"/api/me", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := browser.Do(req); err != nil || resp.StatusCode == http.StatusForbidden {
		t.Errorf("cross-site GET refused: %v %v", resp.StatusCode, err)
	}
}

func TestLoginLimitPerAccount(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	e.post(c, "/api/signup", `{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`, nil)

	bad := `{"email":"ana@example.com","password":"not the password"}`
	good := `{"email":"ANA@example.com","password":"` + goodPassword + `"}`
	for i := range 5 {
		if resp := e.post(c, "/api/login", bad, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d: %d, want 401", i+1, resp.StatusCode)
		}
		e.clock.Advance(time.Minute)
	}
	// The sixth attempt in the window is limited, even with the right password.
	resp := e.post(c, "/api/login", good, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("sixth login: %d, want 429", resp.StatusCode)
	}
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); ra != 10*60 {
		t.Errorf("Retry-After = %q, want 600", resp.Header.Get("Retry-After"))
	}

	// The window ends; the limit resets.
	e.clock.Advance(10 * time.Minute)
	if resp := e.post(c, "/api/login", good, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("login after the window: %d, want 200", resp.StatusCode)
	}
	// And a success clears the count: four more failures don't block.
	for range 4 {
		e.post(c, "/api/login", bad, nil)
	}
	if resp := e.post(c, "/api/login", good, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("login after a success and 4 failures: %d, want 200", resp.StatusCode)
	}
}

func TestLoginLimitPerIP(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for i := range 20 {
		body := `{"email":"user` + strconv.Itoa(i) + `@example.com","password":"` + goodPassword + `"}`
		if resp := e.post(c, "/api/login", body, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login %d: %d, want 401", i+1, resp.StatusCode)
		}
	}
	if resp := e.post(c, "/api/login", `{"email":"fresh@example.com","password":"`+goodPassword+`"}`, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("21st failed login from one address: %d, want 429", resp.StatusCode)
	}
	// Sign-up from that address is limited too.
	if resp := e.post(c, "/api/signup", `{"token":"x","password":"`+goodPassword+`"}`, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("sign-up from a blocked address: %d, want 429", resp.StatusCode)
	}
}

func TestClientIPBehindProxy(t *testing.T) {
	// The test server's peer is 127.0.0.1: make it the trusted edge.
	e := newEnvWith(t, httpapi.Options{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	c := e.client()
	e.post(c, "/api/signup", `{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`, nil)

	for _, tc := range []struct{ xff, want string }{
		{"203.0.113.9", "203.0.113.9"},
		{"6.6.6.6, 203.0.113.9", "203.0.113.9"},   // the client's own claim is ignored
		{"203.0.113.9, 127.0.0.1", "203.0.113.9"}, // trusted hops are skipped
		{"garbage, 203.0.113.9", "203.0.113.9"},   // left of the client doesn't matter
		{"203.0.113.9, garbage", "127.0.0.1"},     // a malformed hop stops the walk
		{"", "127.0.0.1"},
	} {
		e.post(c, "/api/login", `{"email":"ana@example.com","password":"`+goodPassword+`"}`, map[string]string{"X-Forwarded-For": tc.xff})
		var ip string
		if err := e.db.ConnAs("otter_api").QueryRow(t.Context(),
			"SELECT host(created_ip) FROM sessions ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&ip); err != nil {
			t.Fatal(err)
		}
		if ip != tc.want {
			t.Errorf("X-Forwarded-For %q: recorded %s, want %s", tc.xff, ip, tc.want)
		}
	}
}
