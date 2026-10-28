package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	withEnv(t, map[string]string{
		"OTTER_DATABASE_URL":            "postgres://otter@127.0.0.1:1/otter",
		"OTTER_JAILDECK_TOKEN_FILE":     token,
		"OTTER_CLUSTER_ADMIN_CERT_FILE": token,
		"OTTER_CLUSTER_ADMIN_KEY_FILE":  token,
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"worker"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "cluster admin certificate") {
		t.Errorf("run(worker) = %d, stderr %q; want 1 and the certificate refused", code, errOut.String())
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
