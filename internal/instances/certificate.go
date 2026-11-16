package instances

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// ClusterCertName is the name the edge verifies a cluster's certificate by
// (#171): its first DNS subjectAltName. The certificate is self-signed and
// pinned whole, so the name proves nothing more; but Go, and so the edge,
// verifies a name against a subjectAltName only, never the CN.
func ClusterCertName(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("the cluster's certificate is not a PEM certificate")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	if len(c.DNSNames) == 0 {
		return "", errors.New("the cluster's certificate has no DNS subjectAltName, so the edge can't verify it; run otter-pg-bootstrap again in its jail (#171)")
	}
	return c.DNSNames[0], nil
}
