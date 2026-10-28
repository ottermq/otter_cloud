package main

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// migrateEnv points otter migrate at db the way an operator would: the URL
// without a password, the password in a 0400 file.
func migrateEnv(t *testing.T, db *dbtest.DB) map[string]string {
	t.Helper()
	u := db.URL()
	m := map[string]string{}
	if pw, ok := u.User.Password(); ok {
		path := filepath.Join(t.TempDir(), "db-password")
		if err := os.WriteFile(path, []byte(pw), 0o400); err != nil {
			t.Fatal(err)
		}
		m["OTTER_DATABASE_PASSWORD_FILE"] = path
		u.User = url.User(u.User.Username())
	}
	m["OTTER_DATABASE_URL"] = u.String()
	return m
}

func TestMigrate_AppliesThenReportsUpToDate(t *testing.T) {
	db := dbtest.Empty(t)
	withEnv(t, migrateEnv(t, db))

	var out, errOut bytes.Buffer
	if code := run([]string{"migrate"}, &out, &errOut); code != 0 {
		t.Fatalf("migrate = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "applied 0001_privileges") {
		t.Errorf("stdout = %q, want the applied migrations listed", out.String())
	}
	if !strings.Contains(out.String(), "added flavor db-small") {
		t.Errorf("stdout = %q, want the catalog sync listed", out.String())
	}

	out.Reset()
	if code := run([]string{"migrate"}, &out, &errOut); code != 0 {
		t.Fatalf("second migrate = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("stdout = %q, want up to date", out.String())
	}

	out.Reset()
	if code := run([]string{"migrate", "-status"}, &out, &errOut); code != 0 {
		t.Fatalf("migrate -status = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "0001_privileges  applied") {
		t.Errorf("stdout = %q, want 0001 listed as applied", out.String())
	}
}

func TestMigrate_StatusOnFreshDatabaseChangesNothing(t *testing.T) {
	db := dbtest.Empty(t)
	withEnv(t, migrateEnv(t, db))

	var out, errOut bytes.Buffer
	if code := run([]string{"migrate", "-status"}, &out, &errOut); code != 0 {
		t.Fatalf("migrate -status = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "0001_privileges  pending") {
		t.Errorf("stdout = %q, want 0001 pending", out.String())
	}
}
