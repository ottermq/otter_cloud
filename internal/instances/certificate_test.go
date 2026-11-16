package instances_test

import (
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

	"github.com/ottermq/otter_cloud/internal/instances"
)

// certPEM is a self-signed certificate as otter-pg-bootstrap makes one.
func certPEM(t *testing.T, cn string, dns ...string) []byte {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The edge verifies a cluster by its certificate's subjectAltName (#171);
// one named by its CN alone has no name the edge can use.
func TestClusterCertName(t *testing.T) {
	if name, err := instances.ClusterCertName(certPEM(t, "otc-pgc1", "otc-pgc1")); err != nil || name != "otc-pgc1" {
		t.Errorf("with a subjectAltName: %q, %v", name, err)
	}
	if _, err := instances.ClusterCertName(certPEM(t, "otc-pgc1")); err == nil || !strings.Contains(err.Error(), "otter-pg-bootstrap") {
		t.Errorf("CN only: %v, want an error saying what to do", err)
	}
	if _, err := instances.ClusterCertName([]byte("not a certificate")); err == nil {
		t.Error("not PEM: no error")
	}
}
