package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

func TestAdminSubscription_CreateListCancel(t *testing.T) {
	db := dbtest.New(t)
	withEnv(t, adminEnv(t, db))
	ctx := context.Background()
	if _, err := identity.CreateUser(ctx, db.ConnAs(dbtest.RoleAPI), identity.NewUser{
		Email: "ana@example.com", Password: "correct horse battery staple"}); err != nil {
		t.Fatal(err)
	}
	admin := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := run(append([]string{"admin"}, args...), &out, &errOut)
		return out.String(), errOut.String(), code
	}

	out, errOut, code := admin("plan", "list")
	if code != 0 || !strings.Contains(out, "founder-beta") || !strings.Contains(out, "free") {
		t.Fatalf("plan list = %d %q %q", code, out, errOut)
	}

	out, errOut, code = admin("subscription", "create", "-reason", "first founder", "Ana@Example.com", "founder-beta")
	if code != 0 || !strings.Contains(out, "subscribed to Founder Beta (active)") {
		t.Fatalf("subscription create = %d %q %q", code, out, errOut)
	}
	if _, errOut, code = admin("subscription", "create", "ana@example.com", "founder-beta"); code != 1 || !strings.Contains(errOut, "already has a subscription") {
		t.Errorf("second create = %d %q, want refused", code, errOut)
	}
	if _, errOut, code = admin("subscription", "create", "bob@example.com", "founder-beta"); code != 1 || !strings.Contains(errOut, "no user") {
		t.Errorf("unknown user = %d %q", code, errOut)
	}
	if _, errOut, code = admin("subscription", "create", "ana@example.com"); code != 2 {
		t.Errorf("missing plan = %d %q, want usage", code, errOut)
	}

	out, _, code = admin("subscription", "list")
	if code != 0 || !strings.Contains(out, "ana@example.com") || !strings.Contains(out, "active") || !strings.Contains(out, "not_required") {
		t.Errorf("subscription list = %d %q", code, out)
	}

	if out, errOut, code = admin("subscription", "cancel", "ana@example.com"); code != 0 || !strings.Contains(out, "canceled") {
		t.Fatalf("subscription cancel = %d %q %q", code, out, errOut)
	}
	if _, errOut, code = admin("subscription", "cancel", "ana@example.com"); code != 1 || !strings.Contains(errOut, "no subscription") {
		t.Errorf("second cancel = %d %q", code, errOut)
	}

	var actions []string
	rows, err := db.ConnAs(dbtest.RoleAPI).Query(ctx,
		`SELECT action || ':' || coalesce(details->>'reason', '') FROM audit_events WHERE action LIKE 'subscription.%' ORDER BY at, id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	if got := strings.Join(actions, ","); got != "subscription.create:first founder,subscription.cancel:" {
		t.Errorf("audit = %q, want one create (with its reason) and one cancel", got)
	}
}
