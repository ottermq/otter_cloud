package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

func TestAdminInvite_CreateThenList(t *testing.T) {
	db := dbtest.New(t)
	env := migrateEnv(t, db)
	env["OTTER_PUBLIC_URL"] = "https://cloud.example.com"
	withEnv(t, env)

	var out, errOut bytes.Buffer
	if code := run([]string{"admin", "invite", "create", "-ttl", "24h", "Ana@Example.com"}, &out, &errOut); code != 0 {
		t.Fatalf("invite create = %d, stderr: %s", code, errOut.String())
	}
	const prefix = "https://cloud.example.com/signup#invite="
	i := strings.Index(out.String(), prefix)
	if i < 0 || !strings.Contains(out.String(), "ana@example.com") {
		t.Fatalf("stdout = %q, want the link and the address", out.String())
	}
	token := strings.Fields(out.String()[i+len(prefix):])[0]

	var actor, target string
	if err := db.ConnAs(dbtest.RoleAPI).QueryRow(context.Background(),
		"SELECT actor, details->>'email' FROM audit_events WHERE action = 'invite.create'").Scan(&actor, &target); err != nil {
		t.Fatalf("invite.create not audited: %v", err)
	}
	if !strings.HasPrefix(actor, "operator:") || target != "ana@example.com" {
		t.Errorf("audit row: actor %q, email %q", actor, target)
	}

	if _, err := identity.SignUpWithInvite(context.Background(), db.ConnAs(dbtest.RoleAPI),
		identity.SignUp{Token: token, Password: "correct horse battery staple"}); err != nil {
		t.Fatalf("sign up with the printed link: %v", err)
	}

	out.Reset()
	if code := run([]string{"admin", "invite", "list"}, &out, &errOut); code != 0 {
		t.Fatalf("invite list = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ana@example.com") || !strings.Contains(out.String(), "used") {
		t.Errorf("stdout = %q, want the invite listed as used", out.String())
	}
	if strings.Contains(out.String(), token) {
		t.Error("invite list shows the token")
	}
}

func TestAdmin_Usage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"admin"}, 2},
		{[]string{"admin", "-h"}, 0},
		{[]string{"admin", "frobnicate"}, 2},
		{[]string{"admin", "invite"}, 2},
		{[]string{"admin", "invite", "create"}, 2},
		{[]string{"admin", "invite", "create", "a@example.com", "b@example.com"}, 2},
		{[]string{"admin", "invite", "create", "-h"}, 0},
	} {
		var out, errOut bytes.Buffer
		if code := run(tc.args, &out, &errOut); code != tc.code {
			t.Errorf("run(%q) = %d, want %d; stderr: %s", tc.args, code, tc.code, errOut.String())
		}
	}
}

func TestAdminInvite_NeedsPublicURL(t *testing.T) {
	withEnv(t, map[string]string{"OTTER_DATABASE_URL": "postgres://otter@127.0.0.1/otter"})
	var out, errOut bytes.Buffer
	if code := run([]string{"admin", "invite", "create", "ana@example.com"}, &out, &errOut); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "OTTER_PUBLIC_URL") {
		t.Errorf("stderr = %q, want it to name OTTER_PUBLIC_URL", errOut.String())
	}
}
