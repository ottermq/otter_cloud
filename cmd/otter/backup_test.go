package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// otter backup push copies the newest dumps sealed and keeps -keep of them;
// otter backup get, with the identity kept off the node, opens one.
func TestRun_BackupPushAndGet(t *testing.T) {
	tmp := t.TempDir()
	target, dumps := filepath.Join(tmp, "target"), filepath.Join(tmp, "dumps")
	for _, d := range []string{target, dumps} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipients, identity := filepath.Join(tmp, "recipients"), filepath.Join(tmp, "identity")
	if err := os.WriteFile(recipients, []byte(id.Recipient().String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"otter-20261102T030100Z.dump": "monday", "otter-20261103T030100Z.dump": "tuesday",
		"otter-20261104T030100Z.dump": "wednesday", ".otter-20261105T030100Z.dump.tmp": "half", "notes.txt": "not a dump",
	} {
		if err := os.WriteFile(filepath.Join(dumps, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withEnv(t, map[string]string{"OTTER_BACKUP_TARGET": "fs", "OTTER_BACKUP_FS_DIR": target, "OTTER_BACKUP_RECIPIENTS_FILE": recipients})

	push := func() string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := run([]string{"backup", "push", "-dir", dumps, "-keep", "2"}, &out, &errOut); code != 0 {
			t.Fatalf("push = %d: %s", code, errOut.String())
		}
		return out.String()
	}
	if out := push(); strings.Count(out, "copied ") != 2 || strings.Contains(out, "20261102") {
		t.Errorf("first push:\n%s\nwant the newest two copied", out)
	}
	if out := push(); out != "" {
		t.Errorf("second push:\n%s\nwant nothing to do", out)
	}
	if err := os.WriteFile(filepath.Join(dumps, "otter-20261105T030100Z.dump"), []byte("thursday"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := push(); !strings.Contains(out, "copied platform/otter-20261105T030100Z.dump.age") ||
		!strings.Contains(out, "removed platform/otter-20261103T030100Z.dump.age") {
		t.Errorf("third push:\n%s\nwant thursday copied and tuesday removed", out)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"backup", "list"}, &out, &errOut); code != 0 || strings.Count(out.String(), "\n") != 2 {
		t.Errorf("list = %d:\n%s%s", code, out.String(), errOut.String())
	}
	sealed, _ := os.ReadFile(filepath.Join(target, "platform", "otter-20261105T030100Z.dump.age"))
	if bytes.Contains(sealed, []byte("thursday")) {
		t.Error("the copy is not sealed")
	}

	restored := filepath.Join(tmp, "restored.dump")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"backup", "get", "-key", "platform/otter-20261105T030100Z.dump.age", "-identity", identity, "-o", restored}, &out, &errOut); code != 0 {
		t.Fatalf("get = %d: %s", code, errOut.String())
	}
	if b, _ := os.ReadFile(restored); string(b) != "thursday" {
		t.Errorf("restored %q", b)
	}
	if code := run([]string{"backup", "get", "-key", "platform/otter-20261105T030100Z.dump.age", "-identity", identity, "-o", restored}, &out, &errOut); code != 1 {
		t.Errorf("get over an existing file = %d, want 1", code)
	}
}

// The backup commands refuse to run with no target.
func TestRun_BackupWithoutTarget(t *testing.T) {
	withEnv(t, nil)
	var out, errOut bytes.Buffer
	if code := run([]string{"backup", "list"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "OTTER_BACKUP_TARGET is none") {
		t.Errorf("list = %d: %s", code, errOut.String())
	}
}
