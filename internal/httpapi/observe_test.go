package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/httpapi"
)

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(e.client(), "GET", "/healthz", "")
	if resp.StatusCode != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz: %d %v", resp.StatusCode, body)
	}
	e.pool.Close() // the database goes away
	resp, body = e.do(e.client(), "GET", "/healthz", "")
	if resp.StatusCode != http.StatusServiceUnavailable || body["status"] != "unavailable" {
		t.Errorf("healthz without a database: %d %v, want 503", resp.StatusCode, body)
	}
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// logLines returns the JSON log records whose msg is msg.
func (e *env) logLines(msg string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			e.t.Fatalf("log line %q is not JSON", line)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestRequestIDInLogs(t *testing.T) {
	e := newEnv(t)
	c := e.client()

	resp, _ := e.do(c, "GET", "/api/me", "")
	id := resp.Header.Get("X-Request-Id")
	if !hex32.MatchString(id) {
		t.Fatalf("X-Request-Id = %q", id)
	}
	resp2, _ := e.do(c, "GET", "/api/me", "")
	if resp2.Header.Get("X-Request-Id") == id {
		t.Error("two requests share an id")
	}

	lines := e.logLines("request")
	if len(lines) != 2 {
		t.Fatalf("%d access log lines, want 2:\n%s", len(lines), e.logs.String())
	}
	l := lines[0]
	if l["request_id"] != id || l["route"] != "GET /api/me" || l["status"] != float64(401) || l["method"] != "GET" {
		t.Errorf("access log line: %v", l)
	}

	// Other lines in a request carry the id too.
	resp = e.post(c, "/api/logout", "", map[string]string{"Sec-Fetch-Site": "cross-site"})
	refused := e.logLines("cross-origin request refused")
	if len(refused) != 1 || refused[0]["request_id"] != resp.Header.Get("X-Request-Id") {
		t.Errorf("refusal log lines: %v, want one with request_id %s", refused, resp.Header.Get("X-Request-Id"))
	}
}

func TestRequestIDFromTrustedProxyOnly(t *testing.T) {
	header := map[string]string{"X-Request-Id": "edge-abc.123"}

	direct := newEnv(t)
	if got := direct.post(direct.client(), "/api/logout", "", header).Header.Get("X-Request-Id"); got == "edge-abc.123" {
		t.Error("a client chose its own request id")
	}

	behind := newEnvWith(t, httpapi.Options{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if got := behind.post(behind.client(), "/api/logout", "", header).Header.Get("X-Request-Id"); got != "edge-abc.123" {
		t.Errorf("the edge's request id was replaced: %q", got)
	}
	header["X-Request-Id"] = "bad id with spaces"
	if got := behind.post(behind.client(), "/api/logout", "", header).Header.Get("X-Request-Id"); !hex32.MatchString(got) {
		t.Errorf("a malformed id was kept: %q", got)
	}
}

func TestMetrics(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	e.do(c, "GET", "/api/me", "")
	e.do(c, "GET", "/api/me", "")
	e.do(c, "GET", "/healthz", "")
	e.post(c, "/api/logout", "", map[string]string{"Sec-Fetch-Site": "cross-site"})

	rec := httptest.NewRecorder()
	e.handler.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`otter_http_requests_total{code="401",method="GET",route="GET /api/me"} 2`,
		`otter_http_requests_total{code="200",method="GET",route="GET /healthz"} 1`,
		`otter_http_requests_total{code="403",method="POST",route="unmatched"} 1`,
		`otter_http_request_duration_seconds_count{method="GET",route="GET /api/me"} 2`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	// Not served on the public listener.
	resp, err := c.Get(e.server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("public /metrics: %d, want 404", resp.StatusCode)
	}
}
