package postgres_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/clustertest"
	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/jaildeck/jaildecktest"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// testCluster is the test cluster, with this package's helpers.
type testCluster struct {
	clustertest.Cluster
	t *testing.T
}

func cluster(t *testing.T) testCluster { return testCluster{clustertest.New(t), t} }

// The acceptance of #50: the connection works with the node's certificate.
func TestAdminConnects(t *testing.T) {
	c := cluster(t)
	ctx := context.Background()
	conn, err := postgres.Connect(ctx, c.Target(), c.Creds("admin"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var user string
	var ssl bool
	if err := conn.QueryRow(ctx, `SELECT current_user, ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).Scan(&user, &ssl); err != nil || user != postgres.AdminRole || !ssl {
		t.Errorf("connected as %q, ssl %t, %v", user, ssl, err)
	}
}

// The acceptance of #50: a password is not enough, and neither is a
// certificate from another CA, or a cluster that isn't the pinned one.
func TestAdminRefusals(t *testing.T) {
	c := cluster(t)
	ctx := context.Background()
	port := strings.TrimSpace(string(c.File("port")))
	host := strings.TrimSpace(string(c.File("host")))
	for _, mode := range []string{"disable", "require"} {
		url := "postgres://otter_admin:otter-admin-password@" + host + ":" + port + "/postgres?sslmode=" + mode
		if conn, err := pgx.Connect(ctx, url); err == nil {
			conn.Close(ctx)
			t.Errorf("password login with sslmode=%s succeeded", mode)
		} else if !strings.Contains(err.Error(), "pg_hba.conf") && !strings.Contains(err.Error(), "certificate") {
			t.Errorf("password login with sslmode=%s: %v, want refused by pg_hba", mode, err)
		}
	}
	if _, err := postgres.Connect(ctx, c.Target(), c.Creds("stranger-admin")); err == nil {
		t.Error("a certificate from another CA was accepted")
	}
	wrong := c.Target()
	wrong.ServerCert = c.File("stranger.crt")
	if _, err := postgres.Connect(ctx, wrong, c.Creds("admin")); err == nil || !strings.Contains(err.Error(), postgres.ErrServerCert.Error()) {
		t.Errorf("a cluster with another certificate: %v, want ErrServerCert", err)
	}
	if _, err := postgres.LoadCredentials(c.File("tenant.crt"), c.File("tenant.key")); err == nil || !strings.Contains(err.Error(), "CN=otter_admin") {
		t.Errorf("a certificate for another role: %v", err)
	}
	if _, err := postgres.LoadCredentials(c.File("admin.crt"), c.File("stranger-admin.key")); err == nil {
		t.Error("a certificate with someone else's key was loaded")
	}
}

func TestServerCertThroughJailDeck(t *testing.T) {
	pem := selfSigned(t)
	jd := jaildecktest.New(t, jaildeck.Jail{Name: "otc-pg-test", Status: "running"})
	jd.OnExec(func(_ string, argv []string, _ map[string]string, _ time.Duration) jaildeck.ExecResult {
		if strings.Join(argv, " ") == "cat /var/db/postgres/data18/server.crt" {
			return jaildeck.ExecResult{Stdout: pem}
		}
		return jaildeck.ExecResult{ExitCode: 1, Stderr: "No such file or directory"}
	})
	ctx := context.Background()
	got, err := postgres.ServerCert(ctx, jd.Client(), "otc-pg-test", 18)
	if err != nil || string(got) != pem {
		t.Errorf("ServerCert = %q, %v", got, err)
	}
	if _, err := postgres.ServerCert(ctx, jd.Client(), "otc-pg-test", 17); err == nil {
		t.Error("a missing certificate was read")
	}
	jd.OnExec(func(string, []string, map[string]string, time.Duration) jaildeck.ExecResult {
		return jaildeck.ExecResult{Stdout: "not a certificate"}
	})
	if _, err := postgres.ServerCert(ctx, jd.Client(), "otc-pg-test", 18); err == nil {
		t.Error("something that isn't a certificate was pinned")
	}
}

func selfSigned(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "otc-pg-test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
