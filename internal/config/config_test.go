package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPassword = "s3cret-hunter2"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func secretFile(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile's mode is masked by umask
		t.Fatal(err)
	}
	return path
}

func valid(t *testing.T) map[string]string {
	return map[string]string{
		EnvDatabaseURL:          "postgres://otter_api@127.0.0.1:5432/otter?sslmode=disable",
		EnvDatabasePasswordFile: secretFile(t, 0o400, testPassword+"\n"),
		EnvPublicURL:            "https://cloud.otter-labs.com",
	}
}

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load(env(valid(t)), Needs{Database: true, HTTP: true, PublicURL: true})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.DatabasePassword.Reveal(); got != testPassword {
		t.Errorf("password = %q, want the file's contents without the newline", got)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want the default %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
}

func TestLoad_OnlyWhatIsNeeded(t *testing.T) {
	// A worker needs no HTTP settings, so their absence is not an error.
	m := valid(t)
	delete(m, EnvPublicURL)
	if _, err := Load(env(m), Needs{Database: true}); err != nil {
		t.Errorf("Load() error = %v, want nil", err)
	}
	// And `otter version`-like commands need nothing at all.
	if _, err := Load(env(nil), Needs{}); err != nil {
		t.Errorf("Load(empty) error = %v, want nil", err)
	}
}

func TestLoad_ReportsEveryProblemNamingItsVariable(t *testing.T) {
	_, err := Load(env(map[string]string{EnvLogLevel: "loud"}), Needs{Database: true, HTTP: true, PublicURL: true})
	if err == nil {
		t.Fatal("Load() error = nil, want errors")
	}
	for _, name := range []string{EnvLogLevel, EnvDatabaseURL, EnvPublicURL} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s:\n%v", name, err)
		}
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
		want string
	}{
		{"password in URL", EnvDatabaseURL, "postgres://otter_api:" + testPassword + "@db/otter", "must not contain a password"},
		{"password parameter", EnvDatabaseURL, "postgres://otter_api@db/otter?password=" + testPassword, "must not contain a password"},
		{"wrong scheme", EnvDatabaseURL, "mysql://otter_api@db/otter", "scheme must be postgres"},
		{"no user", EnvDatabaseURL, "postgres://db/otter", "no user"},
		{"no database", EnvDatabaseURL, "postgres://otter_api@db", "no database name"},
		{"no host", EnvDatabaseURL, "postgres:///otter", "no host"},
		{"listen not host:port", EnvListenAddr, "8080", "not host:port"},
		{"public URL http", EnvPublicURL, "http://cloud.otter-labs.com", "allowed only for localhost"},
		{"public URL with path", EnvPublicURL, "https://cloud.otter-labs.com/app", "without a path"},
		{"public URL relative", EnvPublicURL, "cloud.otter-labs.com", "not an absolute URL"},
		{"log level", EnvLogLevel, "verbose", "not one of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid(t)
			m[tt.key] = tt.val
			_, err := Load(env(m), Needs{Database: true, HTTP: true, PublicURL: true})
			if err == nil {
				t.Fatalf("Load() error = nil, want one containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.key) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to name %s and say %q", err, tt.key, tt.want)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestLoad_PublicURLHTTPOnLoopback(t *testing.T) {
	for _, u := range []string{"http://localhost:5173", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		m := valid(t)
		m[EnvPublicURL] = u
		if _, err := Load(env(m), Needs{HTTP: true}); err != nil {
			t.Errorf("%s: error = %v, want nil", u, err)
		}
	}
}

func TestLoad_SecretFile(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
		want string
	}{
		{"group readable", func(t *testing.T) string { return secretFile(t, 0o440, testPassword) }, "must not be readable by group or others"},
		{"world readable", func(t *testing.T) string { return secretFile(t, 0o604, testPassword) }, "must not be readable by group or others"},
		{"empty", func(t *testing.T) string { return secretFile(t, 0o600, "\n") }, "is empty"},
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") }, "no such file"},
		{"directory", func(t *testing.T) string { return t.TempDir() }, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid(t)
			m[EnvDatabasePasswordFile] = tt.path(t)
			_, err := Load(env(m), Needs{Database: true})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), EnvDatabasePasswordFile) {
				t.Errorf("error = %v, want it to name %s and say %q", err, EnvDatabasePasswordFile, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), testPassword) {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestSecret_NeverPrints(t *testing.T) {
	s := NewSecret(testPassword)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cfg, err := Load(env(valid(t)), Needs{Database: true, HTTP: true, PublicURL: true})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("startup", "secret", s, "config", cfg)

	encoded, err := json.Marshal(struct{ S Secret }{s})
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", s),
		"%s":   fmt.Sprintf("%s", s),
		"%+v":  fmt.Sprintf("%+v", struct{ S Secret }{s}),
		"%#v":  fmt.Sprintf("%#v", s),
		"%q":   fmt.Sprintf("%q", s),
		"json": string(encoded),
		"slog": logs.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, testPassword) {
			t.Errorf("%s leaks the secret: %s", name, out)
		}
	}
	if s.Reveal() != testPassword {
		t.Error("Reveal() does not return the value")
	}
	if !strings.Contains(logs.String(), `"database_password_set":true`) {
		t.Errorf("config log should say a password is set: %s", logs.String())
	}
}
