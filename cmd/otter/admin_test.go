package main

import (
	"bytes"
	"context"
	"errors"
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
		{[]string{"admin", "user", "disable"}, 2},
		{[]string{"admin", "user", "disable", "-h"}, 0},
		{[]string{"admin", "user", "enable", "a@example.com", "b@example.com"}, 2},
		{[]string{"admin", "user", "disable", "-reason", strings.Repeat("x", 501), "a@example.com"}, 2},
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

func TestAdminUser_DisableEnable(t *testing.T) {
	db := dbtest.New(t)
	withEnv(t, migrateEnv(t, db))
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	const pw = "correct horse battery staple"

	u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	token, err := identity.CreateSession(ctx, api, u.ID, identity.Client{})
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"admin", "user", "disable", "-reason", "lost laptop", "ANA@example.com"}, &out, &errOut); code != 0 {
		t.Fatalf("user disable = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "1 session(s) ended") {
		t.Errorf("stdout = %q", out.String())
	}

	// The acceptance criteria: sessions end, login fails, it's audited.
	if _, err := identity.Authenticate(ctx, api, token); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("session after disable: %v, want ErrNoSession", err)
	}
	if _, err := identity.Login(ctx, api, "ana@example.com", pw); !errors.Is(err, identity.ErrLoginFailed) {
		t.Errorf("login after disable: %v, want ErrLoginFailed", err)
	}
	var actor, reason, revoked string
	if err := api.QueryRow(ctx,
		`SELECT actor, details->>'reason', details->>'sessions_revoked'
		 FROM audit_events WHERE action = 'user.disable' AND target_id = $1`, u.ID).Scan(&actor, &reason, &revoked); err != nil {
		t.Fatalf("user.disable not audited: %v", err)
	}
	if !strings.HasPrefix(actor, "operator:") || reason != "lost laptop" || revoked != "1" {
		t.Errorf("audit: actor %q, reason %q, sessions_revoked %q", actor, reason, revoked)
	}

	out.Reset()
	if code := run([]string{"admin", "user", "list"}, &out, &errOut); code != 0 {
		t.Fatalf("user list = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ana@example.com") || !strings.Contains(out.String(), "disabled") {
		t.Errorf("user list = %q", out.String())
	}

	out.Reset()
	if code := run([]string{"admin", "user", "disable", "ana@example.com"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "already disabled") {
		t.Errorf("second disable = %d, %q", code, out.String())
	}

	out.Reset()
	if code := run([]string{"admin", "user", "enable", "ana@example.com"}, &out, &errOut); code != 0 {
		t.Fatalf("user enable = %d, stderr: %s", code, errOut.String())
	}
	if _, err := identity.Login(ctx, api, "ana@example.com", pw); err != nil {
		t.Errorf("login after enable: %v", err)
	}
	// Enabling doesn't revive the old session.
	if _, err := identity.Authenticate(ctx, api, token); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("old session after enable: %v, want ErrNoSession", err)
	}

	var n int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action IN ('user.disable', 'user.enable')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d audit events, want 2: a no-op disable records nothing", n)
	}

	errOut.Reset()
	if code := run([]string{"admin", "user", "disable", "zed@example.com"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no user") {
		t.Errorf("unknown user = %d, %q", code, errOut.String())
	}
}
