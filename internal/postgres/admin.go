package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// AdminRole is the role the worker administers a cluster as. pg_hba lets it
// in only over TLS, with a client certificate whose CN is this name, from
// the node host's address (architecture.md §6, AD-6); it has no password
// that works.
const AdminRole = "otter_admin"

// Port is where PostgreSQL listens in a cluster jail, on the cluster's
// address. Customers reach PgBouncer instead, on the pool port.
const Port = 5432

// Cluster is how to reach one cluster's PostgreSQL as its admin.
type Cluster struct {
	Host string // the cluster's address, its pool_addr
	Port int    // Port when zero
	// ServerCert is the cluster's own self-signed certificate, PEM. The
	// connection accepts that certificate and no other: it is pinned, read
	// through JailDeck (ServerCert), never learnt from the network.
	ServerCert []byte
}

// Credentials are the node's client certificate for AdminRole, issued by
// the platform's admin CA, whose key stays offline (security.md §5).
type Credentials struct {
	Cert tls.Certificate
}

// LoadCredentials parses the node's client certificate and key, PEM.
func LoadCredentials(certPEM, keyPEM []byte) (Credentials, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Credentials{}, fmt.Errorf("cluster admin certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return Credentials{}, fmt.Errorf("cluster admin certificate: %w", err)
	}
	if leaf.Subject.CommonName != AdminRole {
		return Credentials{}, fmt.Errorf("cluster admin certificate is for %q; PostgreSQL's cert auth needs CN=%s", leaf.Subject.CommonName, AdminRole)
	}
	if time.Now().After(leaf.NotAfter) {
		return Credentials{}, fmt.Errorf("cluster admin certificate expired on %s", leaf.NotAfter.Format(time.DateOnly))
	}
	return Credentials{Cert: c}, nil
}

// ErrServerCert means the cluster presented a certificate other than the
// pinned one.
var ErrServerCert = errors.New("the cluster's certificate is not the one pinned for it")

// Connect opens an admin connection to the cluster's maintenance database.
// Only TLS is tried: there is no fallback to a plain connection.
func Connect(ctx context.Context, c Cluster, creds Credentials) (*pgx.Conn, error) {
	return ConnectDB(ctx, c, creds, "postgres")
}

// ConnectDB opens an admin connection to one database of the cluster, for
// the steps that run inside it (REVOKE … ON SCHEMA public).
func ConnectDB(ctx context.Context, c Cluster, creds Credentials, database string) (*pgx.Conn, error) {
	pinned, err := parseCert(c.ServerCert)
	if err != nil {
		return nil, err
	}
	port := c.Port
	if port == 0 {
		port = Port
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Database = c.Host, uint16(port), AdminRole, database
	cfg.Password = "" // never, even if PGPASSWORD is set: the certificate is the credential
	cfg.ConnectTimeout = 10 * time.Second
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{"application_name": "otter-worker"}
	cfg.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{creds.Cert},
		MinVersion:   tls.VersionTLS13,
		// The chain is not checked: the cluster's certificate is
		// self-signed, and it is compared whole with the pinned one below.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, pinned.Raw) {
				return ErrServerCert
			}
			return nil
		},
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to cluster %s as %s: %w", net.JoinHostPort(c.Host, strconv.Itoa(port)), AdminRole, err)
	}
	return conn, nil
}

func parseCert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the cluster's pinned certificate is not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ServerCert reads the cluster's certificate from its jail through
// JailDeck, which is the trusted path on the node: the certificate is never
// taken from the network connection it is meant to authenticate.
func ServerCert(ctx context.Context, jd *jaildeck.Client, jail string, pgMajor int) ([]byte, error) {
	path := fmt.Sprintf("/var/db/postgres/data%d/server.crt", pgMajor)
	r, err := jd.Exec(ctx, jail, jaildeck.ExecRequest{Argv: []string{"cat", path}, Timeout: 10 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("read %s in %s: %w", path, jail, err)
	}
	if r.ExitCode != 0 || r.StdoutTruncated {
		return nil, fmt.Errorf("read %s in %s: exit %d: %s", path, jail, r.ExitCode, r.Stderr)
	}
	if _, err := parseCert([]byte(r.Stdout)); err != nil {
		return nil, fmt.Errorf("read %s in %s: %w", path, jail, err)
	}
	return []byte(r.Stdout), nil
}
