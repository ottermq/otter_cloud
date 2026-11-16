package httpapi_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"testing"
	"time"

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

	// None to a cluster whose certificate the worker hasn't recorded, or
	// that the edge can't verify (#171).
	setCert := func(cert string) {
		t.Helper()
		if _, err := mig.Exec(ctx, `UPDATE clusters SET server_cert = $1 WHERE name = 'pgc1'`, cert); err != nil {
			t.Fatal(err)
		}
	}
	if code, got := edgeRoutes(t, e.handler); code != http.StatusOK || len(got) != 0 {
		t.Errorf("no certificate recorded: %d %v, want an empty configuration", code, got)
	}
	_, cnOnly := selfSigned(t, "otc-pgc1")
	setCert(cnOnly)
	if code, got := edgeRoutes(t, e.handler); code != http.StatusOK || len(got) != 0 {
		t.Errorf("a certificate named by its CN only: %d %v, want an empty configuration", code, got)
	}
	_, cert := selfSigned(t, "otc-pgc1", "otc-pgc1")
	setCert(cert)

	code, got := edgeRoutes(t, e.handler)
	want := map[string]any{"tcp": map[string]any{
		"routers": map[string]any{infra: map[string]any{
			"entryPoints": []any{"postgres"},
			"rule":        "HostSNI(`" + host + "`)",
			"service":     "pgc1",
			"tls":         map[string]any{"options": "postgres@file"},
		}},
		"services": map[string]any{"pgc1": map[string]any{"loadBalancer": map[string]any{
			"serversTransport": "pgbouncer-pgc1",
			"servers":          []any{map[string]any{"address": "10.64.0.10:6432", "tls": true}},
		}}},
		"serversTransports": map[string]any{"pgbouncer-pgc1": map[string]any{"tls": map[string]any{
			"serverName": "otc-pgc1", "rootCAs": []any{cert}}}},
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

// The acceptance of #171: the transport's settings, as Go's TLS takes them
// on the edge, accept the cluster's certificate, and a different one on
// the pool address breaks the route, even with the same name.
func TestEdgeTransportPinsTheCertificate(t *testing.T) {
	e := newEnvWith(t, httpapi.Options{DBDomain: "db.example.com",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	e.withCluster()
	ana, _ := e.customer("ana@example.com")
	_, body := e.do(ana, "POST", "/api/instances", `{"name":"running","flavor":"db-small"}`)
	ctx := context.Background()
	pinned, cert := selfSigned(t, "otc-pgc1", "otc-pgc1")
	stranger, _ := selfSigned(t, "otc-pgc1", "otc-pgc1")
	mig := e.db.ConnAs(dbtest.RoleMigrator)
	if _, err := mig.Exec(ctx, `UPDATE instances SET status = 'running' WHERE id = $1`, obj(body["instance"])["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `UPDATE clusters SET server_cert = $1 WHERE name = 'pgc1'`, cert); err != nil {
		t.Fatal(err)
	}
	_, got := edgeRoutes(t, e.handler)
	tr := obj(obj(obj(obj(got["tcp"])["serversTransports"])["pgbouncer-pgc1"])["tls"])
	roots := x509.NewCertPool()
	for _, ca := range tr["rootCAs"].([]any) {
		if !roots.AppendCertsFromPEM([]byte(ca.(string))) {
			t.Fatalf("rootCAs %v: not PEM", tr["rootCAs"])
		}
	}
	client := &tls.Config{RootCAs: roots, ServerName: tr["serverName"].(string)}

	if err := handshake(t, pinned, client); err != nil {
		t.Errorf("to the cluster's PgBouncer: %v", err)
	}
	if err := handshake(t, stranger, client); err == nil {
		t.Error("to another certificate on the pool address: verified, want the route broken")
	}
}

// selfSigned is a certificate as otter-pg-bootstrap makes one, and its PEM.
func selfSigned(t *testing.T, cn string, dns ...string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: dns,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// handshake is the edge's TLS to a server presenting cert.
func handshake(t *testing.T, cert tls.Certificate, client *tls.Config) error {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	c, err := tls.Dial("tcp", l.Addr().String(), client)
	if err != nil {
		return err
	}
	return c.Close()
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
