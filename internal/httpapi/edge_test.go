package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/httpapi"
)

// edgeRoutes fetches the edge's routes as the edge would, from peer.
func edgeRoutes(t *testing.T, h *httpapi.Server) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(h.EdgeHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/traefik")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("body %q is not JSON", data)
	}
	return resp.StatusCode, out
}

// The acceptance of #103: a route per running database, by SNI, to its
// cluster's PgBouncer, and nothing else in it.
func TestEdgeRoutes(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{DBDomain: "db.example.com",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	e.withCluster()

	if code, got := edgeRoutes(t, e.handler); code != http.StatusOK || len(got) != 0 {
		t.Errorf("no databases: %d %v, want an empty configuration", code, got)
	}

	ana, _ := e.customer("ana@example.com")
	bob, _ := e.customer("bob@example.com")
	ids := map[string]string{}
	for _, c := range []struct {
		browser *http.Client
		name    string
	}{{ana, "running"}, {ana, "creating"}, {bob, "blocked"}, {bob, "stopped"}} {
		_, body := e.do(c.browser, "POST", "/api/instances", `{"name":"`+c.name+`","flavor":"db-small"}`)
		ids[c.name] = obj(body["instance"])["id"].(string)
	}
	ctx := context.Background()
	mig := e.db.ConnAs(dbtest.RoleMigrator)
	for name, set := range map[string]string{
		"running": "status = 'running'",
		"blocked": "status = 'running', storage_state = 'blocked'",
		"stopped": "status = 'stopped', desired_state = 'stopped'",
	} {
		if _, err := mig.Exec(ctx, `UPDATE instances SET `+set+` WHERE id = $1`, ids[name]); err != nil {
			t.Fatal(err)
		}
	}
	var infra, host string
	if err := mig.QueryRow(ctx, `SELECT infra_name, endpoint_host FROM instances WHERE id = $1`, ids["running"]).Scan(&infra, &host); err != nil {
		t.Fatal(err)
	}

	code, got := edgeRoutes(t, e.handler)
	want := map[string]any{"tcp": map[string]any{
		"routers": map[string]any{infra: map[string]any{
			"entryPoints": []any{"postgres"},
			"rule":        "HostSNI(`" + host + "`)",
			"service":     "pgc1",
			"tls":         map[string]any{},
		}},
		"services": map[string]any{"pgc1": map[string]any{"loadBalancer": map[string]any{
			"serversTransport": "pgbouncer",
			"servers":          []any{map[string]any{"address": "10.64.0.10:6432", "tls": true}},
		}}},
		"serversTransports": map[string]any{"pgbouncer": map[string]any{"tls": map[string]any{"insecureSkipVerify": true}}},
	}}
	if code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Errorf("routes: %d\n got %v\nwant %v", code, got, want)
	}

	// The customer sees the same name.
	_, view := e.do(ana, "GET", "/api/instances/"+ids["running"], "")
	if conn := obj(view["connection"]); conn["host"] != host || conn["port"] != 5432.0 {
		t.Errorf("connection: %v, want %s:5432", view["connection"], host)
	}
}

// Only the edge reads the routes, whatever reaches the listener.
func TestEdgeRoutesOnlyToTheEdge(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{DBDomain: "db.example.com",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.62.0.1/32")}})
	if code, got := edgeRoutes(t, e.handler); code != http.StatusForbidden || got["error"] == nil {
		t.Errorf("from 127.0.0.1: %d %v, want 403", code, got)
	}
	// Nor is it on the API's listener.
	resp, err := e.client().Get(e.server.URL + "/traefik")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("on the API's listener: %d, want 404", resp.StatusCode)
	}
}
