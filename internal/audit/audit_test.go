package audit_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/dbtest"
)

func TestRecord(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)

	if err := audit.Record(ctx, api, audit.Event{
		Actor:     audit.Anonymous,
		Action:    audit.ActionLogin,
		IP:        netip.MustParseAddr("203.0.113.9"),
		UserAgent: strings.Repeat("x", 600),
		Outcome:   audit.Denied,
		Details:   map[string]any{"email": "ana@example.com", "reason": "bad_credentials"},
	}); err != nil {
		t.Fatal(err)
	}
	var actor, action, ip, outcome, reason string
	var uaLen int
	var accountNull, targetNull bool
	if err := api.QueryRow(ctx,
		`SELECT actor, action, host(ip), length(user_agent), outcome, details->>'reason',
		        account_id IS NULL, target_type IS NULL
		 FROM audit_events`).Scan(&actor, &action, &ip, &uaLen, &outcome, &reason, &accountNull, &targetNull); err != nil {
		t.Fatal(err)
	}
	if actor != "anonymous" || action != "auth.login" || ip != "203.0.113.9" || uaLen != 512 ||
		outcome != "denied" || reason != "bad_credentials" || !accountNull || !targetNull {
		t.Errorf("row: %s %s %s ua=%d %s %s account_null=%v target_null=%v",
			actor, action, ip, uaLen, outcome, reason, accountNull, targetNull)
	}

	if err := audit.Record(ctx, api, audit.Event{Actor: audit.System, Action: "test.no_details", Outcome: audit.OK}); err != nil {
		t.Errorf("event without details or request: %v", err)
	}
}

func TestRecordRefusesMalformedEvents(t *testing.T) {
	db := dbtest.New(t)
	api := db.ConnAs(dbtest.RoleAPI)
	for name, e := range map[string]audit.Event{
		"bad actor":       {Actor: "root", Action: "auth.login", Outcome: audit.OK},
		"bad action":      {Actor: audit.System, Action: "Login!", Outcome: audit.OK},
		"bad outcome":     {Actor: audit.System, Action: "auth.login", Outcome: "maybe"},
		"half a target":   {Actor: audit.System, Action: "auth.login", Outcome: audit.OK, TargetType: "user"},
		"huge details":    {Actor: audit.System, Action: "auth.login", Outcome: audit.OK, Details: map[string]any{"x": strings.Repeat("a", 10000)}},
		"unknown account": {Actor: audit.System, Action: "auth.login", Outcome: audit.OK, AccountID: "01a17ac7-15fe-761d-a51b-0280014312f6"},
	} {
		if err := audit.Record(context.Background(), api, e); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
}

// The acceptance test for #35: the table is append-only for the
// application roles.
func TestAppendOnly(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	worker := db.ConnAs(dbtest.RoleWorker)

	if err := audit.Record(ctx, api, audit.Event{Actor: audit.System, Action: "test.api", Outcome: audit.OK}); err != nil {
		t.Fatalf("api insert: %v", err)
	}
	if err := audit.Record(ctx, worker, audit.Event{Actor: audit.System, Action: "test.worker", Outcome: audit.OK}); err != nil {
		t.Fatalf("worker insert: %v", err)
	}

	for _, tc := range []struct {
		role string
		sql  string
	}{
		{"api", "UPDATE audit_events SET outcome = 'ok'"},
		{"api", "DELETE FROM audit_events"},
		{"api", "TRUNCATE audit_events"},
		{"worker", "UPDATE audit_events SET outcome = 'ok'"},
		{"worker", "DELETE FROM audit_events"},
		{"worker", "SELECT FROM audit_events"},
	} {
		conn := api
		if tc.role == "worker" {
			conn = worker
		}
		_, err := conn.Exec(ctx, tc.sql)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
			t.Errorf("%s: %s: err = %v, want permission denied", tc.role, tc.sql, err)
		}
	}

	var n int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM audit_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d events, want 2", n)
	}
}

func TestOperator(t *testing.T) {
	if a := audit.Operator(); !strings.HasPrefix(a, "operator:") || len(a) <= len("operator:") {
		t.Errorf("Operator() = %q", a)
	}
}
