package httpapi_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ottermq/otter_cloud/internal/httpapi"
)

const wantCSP = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

var portalFiles = fstest.MapFS{
	"index.html":        {Data: []byte("<!doctype html><title>portal</title>")},
	"favicon.svg":       {Data: []byte("<svg/>")},
	"assets/app-1a.js":  {Data: []byte("console.log(1)")},
	"assets/app-1a.css": {Data: []byte("body{}")},
	"assets/sub/x.js":   {Data: []byte("nested")},
}

func (e *env) get(path string) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.client().Get(e.server.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// The acceptance of #108: the portal's own URLs answer with the page, under
// the CSP, and everything else stays a 404.
func TestPortal(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{Assets: portalFiles})

	for _, path := range []string{"/", "/login", "/signup", "/reset-password", "/databases",
		"/databases/0190a/connection", "/activity", "/subscription", "/account", "/account/security"} {
		resp, body := e.get(path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "portal") {
			t.Errorf("%s: %d %q, want the page", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", path, ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q: a new release's page must be seen at once", path, cc)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); csp != wantCSP {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}

	for path, ct := range map[string]string{"/assets/app-1a.js": "text/javascript", "/assets/app-1a.css": "text/css"} {
		resp, _ := e.get(path)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ct) {
			t.Errorf("%s: %d %q, want %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), ct)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
	}
	if resp, _ := e.get("/favicon.svg"); resp.StatusCode != http.StatusOK {
		t.Errorf("/favicon.svg: %d", resp.StatusCode)
	}

	for _, path := range []string{"/metrics", "/traefik", "/index.html", "/nope", "/.env", "/assets", "/assets/",
		"/assets/sub", "/assets/sub/x.js", "/assets/nope.js", "/assets/..%2findex.html", "/loginx"} {
		resp, body := e.get(path)
		if resp.StatusCode == http.StatusOK || strings.Contains(body, "portal") {
			t.Errorf("%s: %d %q, want it refused", path, resp.StatusCode, body)
		}
		if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
			t.Errorf("%s: a refusal must not be cached: %q", path, cc)
		}
	}
}

// The portal changes nothing about the API: still JSON, still uncached,
// under the same CSP, and still its own 404s.
func TestPortalLeavesTheAPIAlone(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{Assets: portalFiles})
	for path, want := range map[string]int{"/api/me": http.StatusUnauthorized, "/api/nope": http.StatusNotFound} {
		resp, body := e.get(path)
		if resp.StatusCode != want || !strings.Contains(body, `"error"`) {
			t.Errorf("%s: %d %q, want %d and a JSON error", path, resp.StatusCode, body, want)
		}
		if cc, csp := resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Security-Policy"); cc != "no-store" || csp != wantCSP {
			t.Errorf("%s: Cache-Control %q, CSP %q", path, cc, csp)
		}
	}
	if resp, _ := e.get("/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %d", resp.StatusCode)
	}

	// Pages take no writes, and the cross-origin rules still come first.
	public, _ := url.Parse("https://cloud.example.com")
	e = newEnvWith(t, httpapi.Options{Assets: portalFiles, PublicURL: public})
	browser := e.client()
	if resp := e.post(browser, "/login", `{}`, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /login: %d, want 405", resp.StatusCode)
	}
	if resp := e.post(browser, "/api/login", `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-site POST: %d, want 403", resp.StatusCode)
	}
}

// A binary built without make web still serves the API.
func TestNoPortal(t *testing.T) {
	for name, assets := range map[string]fstest.MapFS{"none": nil, "unbuilt": {".gitkeep": {}}} {
		t.Run(name, func(t *testing.T) {
			opts := httpapi.Options{}
			if assets != nil {
				opts.Assets = assets
			}
			e := newEnvWith(t, opts)
			if resp, _ := e.get("/"); resp.StatusCode != http.StatusNotFound {
				t.Errorf("/: %d, want 404", resp.StatusCode)
			}
			if resp, _ := e.get("/api/me"); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("/api/me: %d, want 401", resp.StatusCode)
			}
		})
	}
}
