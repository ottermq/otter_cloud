package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun_NoArgsPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("run() = %d, want 2", code)
	}
	for _, c := range commands() {
		if !strings.Contains(errOut.String(), c.name) {
			t.Errorf("usage does not list %q:\n%s", c.name, errOut.String())
		}
	}
}

func TestRun_HelpGoesToStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-h"}, &out, &errOut); code != 0 {
		t.Errorf("run(-h) = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage: otter <command>") {
		t.Errorf("stdout = %q, want the usage", out.String())
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("run(frobnicate) = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q, want it to name the command", errOut.String())
	}
}

func TestRun_EverySubcommandPrintsItsUsage(t *testing.T) {
	for _, c := range commands() {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run([]string{c.name, "-h"}, &out, &errOut); code != 0 {
				t.Errorf("run(%s -h) = %d, want 0", c.name, code)
			}
			if want := "Usage: otter " + c.name; !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr = %q, want %q", errOut.String(), want)
			}
		})
	}
}

func TestRun_SubcommandRejectsStrayArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"migrate", "extra"}, &out, &errOut); code != 2 {
		t.Errorf("run(migrate extra) = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unexpected argument "extra"`) {
		t.Errorf("stderr = %q, want it to name the argument", errOut.String())
	}
}

// withEnv replaces the environment run reads for the length of a test.
func withEnv(t *testing.T, m map[string]string) {
	t.Helper()
	old := getenv
	t.Cleanup(func() { getenv = old })
	getenv = func(k string) string { return m[k] }
}

func TestRun_InvalidConfigurationNamesTheVariable(t *testing.T) {
	withEnv(t, nil)
	var out, errOut bytes.Buffer
	if code := run([]string{"api"}, &out, &errOut); code != 1 {
		t.Errorf("run(api) = %d, want 1", code)
	}
	for _, want := range []string{"invalid configuration", "OTTER_DATABASE_URL", "OTTER_PUBLIC_URL"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", errOut.String(), want)
		}
	}
}

// The worker checks its cluster admin certificate before it connects to
// anything.
func TestRun_WorkerRefusesABadCertificate(t *testing.T) {
	token := filepath.Join(t.TempDir(), "jaildeck-token")
	if err := os.WriteFile(token, []byte("jdt_test\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	dumps := t.TempDir()
	if err := os.Chmod(dumps, 0o700); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"OTTER_DATABASE_URL":            "postgres://otter@127.0.0.1:1/otter",
		"OTTER_JAILDECK_TOKEN_FILE":     token,
		"OTTER_CLUSTER_ADMIN_CERT_FILE": token,
		"OTTER_CLUSTER_ADMIN_KEY_FILE":  token,
		"OTTER_DUMP_DIR":                dumps,
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"worker"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "cluster admin certificate") {
		t.Errorf("run(worker) = %d, stderr %q; want 1 and the certificate refused", code, errOut.String())
	}
}

// The acceptance of #48: the worker refuses to start when a secret is
// readable by others. rc.d runs this check before it starts the worker.
func TestRun_WorkerCheck(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "otter_admin"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	files := map[string][]byte{
		"cert":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"key":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"token": []byte("jdt_test\n"),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	dumps := filepath.Join(dir, "dumps")
	if err := os.Mkdir(dumps, 0o700); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"OTTER_DATABASE_URL":            "postgres://otter_node_a@127.0.0.1:1/otter",
		"OTTER_JAILDECK_TOKEN_FILE":     filepath.Join(dir, "token"),
		"OTTER_CLUSTER_ADMIN_CERT_FILE": filepath.Join(dir, "cert"),
		"OTTER_CLUSTER_ADMIN_KEY_FILE":  filepath.Join(dir, "key"),
		"OTTER_DUMP_DIR":                dumps,
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"worker", "-check"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "configuration ok") {
		t.Fatalf("check = %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	for _, tc := range []struct{ file, env string }{{"key", "OTTER_CLUSTER_ADMIN_KEY_FILE"}, {"token", "OTTER_JAILDECK_TOKEN_FILE"}, {"dumps", "OTTER_DUMP_DIR"}} {
		path := filepath.Join(dir, tc.file)
		info, _ := os.Stat(path)
		if err := os.Chmod(path, info.Mode().Perm()|0o044); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		errOut.Reset()
		if code := run([]string{"worker", "-check"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), tc.env) {
			t.Errorf("%s readable by others: check = %d, stderr %q", tc.file, code, errOut.String())
		}
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

// rc.d runs `otter api -check` before it starts the api in its jail (#58):
// a password file readable by others stops the start.
func TestRun_APICheck(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "db-password")
	if err := os.WriteFile(pw, []byte("secret\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"OTTER_DATABASE_URL":           "postgres://otter_api@127.0.0.1:1/otter?sslmode=verify-ca",
		"OTTER_DATABASE_PASSWORD_FILE": pw,
		"OTTER_PUBLIC_URL":             "https://cloud.example.org",
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"api", "-check"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "configuration ok") {
		t.Fatalf("check = %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if err := os.Chmod(pw, 0o444); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"api", "-check"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "OTTER_DATABASE_PASSWORD_FILE") {
		t.Errorf("password file readable by others: check = %d, stderr %q", code, errOut.String())
	}
}

func TestRun_Version(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Errorf("run(version) = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("version printed nothing")
	}
}
