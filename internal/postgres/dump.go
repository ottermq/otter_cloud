package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Dumper runs pg_dump on the node host, as the worker's user, to a
// directory only that user can read (Q26). It connects as AdminRole with
// the node's client certificate, over TLS 1.3, trusting the cluster's
// pinned certificate and nothing else.
type Dumper struct {
	// Command is pg_dump and any arguments before pg_dump's own; "pg_dump"
	// from PATH when empty. A test runs it in a container.
	Command []string
	// CertFile and KeyFile are the node's client certificate and key, which
	// libpq reads itself.
	CertFile, KeyFile string
	Dir               string
}

// Dump is a dump file Dumper made.
type Dump struct {
	File   string // its name in Dir
	Size   int64
	SHA256 string
}

// Dump writes database in pg_dump's custom format to Dir/name. The file
// appears only once pg_dump has succeeded and it is on disk: a failed or
// interrupted dump leaves nothing under name.
func (d Dumper) Dump(ctx context.Context, c Cluster, database, name string) (Dump, error) {
	if !namePattern.MatchString(database) || filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return Dump{}, fmt.Errorf("dump %q as %q: not a tenant database and a file name", database, name)
	}
	if _, err := parseCert(c.ServerCert); err != nil {
		return Dump{}, err
	}
	final := filepath.Join(d.Dir, name)
	root := filepath.Join(d.Dir, "."+name+".root.crt")
	if err := writePrivate(root, c.ServerCert); err != nil {
		return Dump{}, err
	}
	defer os.Remove(root)
	tmp := filepath.Join(d.Dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return Dump{}, err
	}
	defer os.Remove(tmp) // gone once renamed
	defer f.Close()

	port := c.Port
	if port == 0 {
		port = Port
	}
	conninfo := strings.Join([]string{
		kv("host", c.Host), kv("port", strconv.Itoa(port)), kv("dbname", database), kv("user", AdminRole),
		// verify-ca against the cluster's own self-signed certificate: only
		// the holder of its key passes, which is the pin.
		kv("sslmode", "verify-ca"), kv("sslrootcert", root), kv("ssl_min_protocol_version", "TLSv1.3"),
		kv("sslcert", d.CertFile), kv("sslkey", d.KeyFile), kv("application_name", "otter-worker-dump"),
	}, " ")
	argv := d.Command
	if len(argv) == 0 {
		argv = []string{"pg_dump"}
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "--format=custom", "--no-password", "--dbname="+conninfo)...)
	// Nothing from the worker's environment: no PGPASSWORD, PGSERVICE or
	// PGSSLMODE can change where it connects or how.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PGCONNECT_TIMEOUT=10"}
	h := sha256.New()
	counter := &countingWriter{}
	cmd.Stdout = io.MultiWriter(f, h, counter)
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Run(); err != nil {
		return Dump{}, fmt.Errorf("pg_dump %s: %w: %s", database, err, strings.TrimSpace(stderr.String()))
	}
	if counter.n == 0 {
		return Dump{}, fmt.Errorf("pg_dump %s wrote nothing", database)
	}
	if err := f.Sync(); err != nil {
		return Dump{}, err
	}
	if err := f.Close(); err != nil {
		return Dump{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return Dump{}, err
	}
	if dir, err := os.Open(d.Dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return Dump{File: name, Size: counter.n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Check reports whether the dump is still in Dir as it was made.
func (d Dumper) Check(dump Dump) error {
	f, err := os.Open(filepath.Join(d.Dir, dump.File))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != dump.Size || hex.EncodeToString(h.Sum(nil)) != dump.SHA256 {
		return fmt.Errorf("dump %s is not what was written: %d bytes", dump.File, n)
	}
	return nil
}

// kv is one libpq conninfo setting, quoted.
func kv(k, v string) string {
	return k + "='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

func writePrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return errors.Join(err, f.Close())
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// limitedWriter keeps the first n bytes and drops the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
