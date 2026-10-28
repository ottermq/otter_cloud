package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/httpapi"
	"github.com/ottermq/otter_cloud/internal/identity"
)

const (
	goodPassword  = "correct horse battery staple"
	sessionCookie = "__Host-otter_session"
)

type env struct {
	t       *testing.T
	db      *dbtest.DB
	pool    *pgxpool.Pool
	handler *httpapi.Server
	server  *httptest.Server
	clock   *clock
	logs    *syncBuffer
}

// syncBuffer collects the server's JSON log lines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// clock is a settable time for the rate limits.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newEnv serves the API over TLS, so the Secure cookie travels as it does
// in production, with the pool acting as otter_api.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, httpapi.Options{})
}

func newEnvWith(t *testing.T, opts httpapi.Options) *env {
	t.Helper()
	db := dbtest.New(t)
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &clock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	opts.SetClock(c.Now)
	pool := db.PoolAs(dbtest.RoleAPI)
	h, err := httpapi.New(pool, log, opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, db: db, pool: pool, handler: h, server: srv, clock: c, logs: logs}
}

// client is a browser: its own cookie jar.
func (e *env) client() *http.Client {
	c := e.server.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	c.Jar = jar
	return c
}

func (e *env) invite(email string) string {
	e.t.Helper()
	_, token, err := identity.CreateInvite(context.Background(), e.db.ConnAs(dbtest.RoleAPI), email, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

func (e *env) do(c *http.Client, method, path, body string) (*http.Response, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			e.t.Fatalf("%s %s: body %q is not JSON", method, path, data)
		}
	}
	return resp, out
}

func findCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestSignUpLoginMeLogout(t *testing.T) {
	e := newEnv(t)
	browser := e.client()

	// Sign up with the invite: logged in straight away.
	resp, body := e.do(browser, "POST", "/api/signup",
		`{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`","account_name":"Ana's Lab"}`)
	if resp.StatusCode != http.StatusCreated || body["email"] != "ana@example.com" {
		t.Fatalf("signup: %d %v", resp.StatusCode, body)
	}
	c := findCookie(resp)
	if c == nil {
		t.Fatal("signup set no session cookie")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("cookie flags: %+v", c)
	}
	if c.MaxAge != int((30 * 24 * time.Hour).Seconds()) {
		t.Errorf("cookie Max-Age = %d, want 30 days", c.MaxAge)
	}

	resp, body = e.do(browser, "GET", "/api/me", "")
	if resp.StatusCode != http.StatusOK || body["email"] != "ana@example.com" || body["is_operator"] != false {
		t.Fatalf("me: %d %v", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}

	// Logout revokes server-side: the old cookie, replayed, is refused.
	stolen := c.Value
	resp, _ = e.do(browser, "POST", "/api/logout", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if cleared := findCookie(resp); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("logout did not clear the cookie: %+v", cleared)
	}
	if resp, _ := e.do(browser, "GET", "/api/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after logout: %d, want 401", resp.StatusCode)
	}
	replay := e.server.Client()
	req, _ := http.NewRequest("GET", e.server.URL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: stolen})
	if resp, err := replay.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed cookie after logout: %v %v, want 401", resp.StatusCode, err)
	}

	// Log in again.
	resp, body = e.do(browser, "POST", "/api/login", `{"email":"Ana@Example.com","password":"`+goodPassword+`"}`)
	if resp.StatusCode != http.StatusOK || body["email"] != "ana@example.com" {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do(browser, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("me after login: %d", resp.StatusCode)
	}
}

func TestMeWithoutSession(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(e.client(), "GET", "/api/me", "")
	if resp.StatusCode != http.StatusUnauthorized || body["error"] == nil {
		t.Errorf("me: %d %v, want 401 with an error", resp.StatusCode, body)
	}
}

func TestLoginRotatesSession(t *testing.T) {
	e := newEnv(t)
	browser := e.client()
	resp, _ := e.do(browser, "POST", "/api/signup", `{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`)
	before := findCookie(resp)

	resp, _ = e.do(browser, "POST", "/api/login", `{"email":"ana@example.com","password":"`+goodPassword+`"}`)
	after := findCookie(resp)
	if before == nil || after == nil || before.Value == after.Value {
		t.Fatal("login did not issue a new session token")
	}
	req, _ := http.NewRequest("GET", e.server.URL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: before.Value})
	if resp, err := e.server.Client().Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the pre-login session still works: %v %v", resp.StatusCode, err)
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t)
	browser := e.client()
	e.do(browser, "POST", "/api/signup", `{"token":"`+e.invite("ana@example.com")+`","password":"`+goodPassword+`"}`)
	e.do(browser, "POST", "/api/logout", "")

	for _, body := range []string{
		`{"email":"ana@example.com","password":"wrong password here"}`,
		`{"email":"zed@example.com","password":"` + goodPassword + `"}`,
	} {
		resp, out := e.do(browser, "POST", "/api/login", body)
		if resp.StatusCode != http.StatusUnauthorized || out["error"] != identity.ErrLoginFailed.Error() {
			t.Errorf("login %s: %d %v", body, resp.StatusCode, out)
		}
		if findCookie(resp) != nil {
			t.Errorf("failed login set a cookie")
		}
	}
}

func TestSignUpFailures(t *testing.T) {
	e := newEnv(t)
	token := e.invite("ana@example.com")
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"token":"nope","password":"` + goodPassword + `"}`, http.StatusBadRequest},
		{`{"token":"` + token + `","password":"short"}`, http.StatusBadRequest},
		{`{"token":"` + token + `","password":"qwertyuiop123"}`, http.StatusBadRequest},
	} {
		resp, out := e.do(e.client(), "POST", "/api/signup", tc.body)
		if resp.StatusCode != tc.code || out["error"] == nil {
			t.Errorf("signup %s: %d %v, want %d", tc.body, resp.StatusCode, out, tc.code)
		}
	}
}

func TestRequestFormat(t *testing.T) {
	e := newEnv(t)
	c := e.client()

	req, _ := http.NewRequest("POST", e.server.URL+"/api/login", strings.NewReader("email=a&password=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := c.Do(req); err != nil || resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %v %v, want 415", resp.StatusCode, err)
	}
	for _, body := range []string{
		`{"email":"a@example.com","password":"x","admin":true}`,
		`{"email":"a@example.com"} {}`,
		`not json`,
		`{"email":"` + strings.Repeat("a", 70<<10) + `"}`,
	} {
		if resp, _ := e.do(c, "POST", "/api/login", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %.40q: %d, want 400", body, resp.StatusCode)
		}
	}
	if resp, body := e.do(c, "GET", "/api/nope", ""); resp.StatusCode != http.StatusNotFound || body["error"] == nil {
		t.Errorf("unknown endpoint: %d %v", resp.StatusCode, body)
	}
	// A wrong method falls through to the JSON 404 too.
	if resp, body := e.do(c, "GET", "/api/login", ""); resp.StatusCode != http.StatusNotFound || body["error"] == nil {
		t.Errorf("GET /api/login: %d %v, want a JSON 404", resp.StatusCode, body)
	}
}
