package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

// exec runs sql as otter_migrator, the tables' owner, to set up rows at
// any age.
func exec(t *testing.T, db *dbtest.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *dbtest.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPurgePersonalData(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}

	// Sessions, by user agent: which go and which stay.
	session := func(ua, created, seen, revoked string) {
		exec(t, db, `INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, revoked_at, created_ip, user_agent)
			VALUES (sha256(convert_to($2, 'UTF8')), $1, now() - $3::interval, now() - $4::interval,
			        now() - nullif($5, '')::interval, '203.0.113.7', $2)`, u.ID, ua, created, seen, revoked)
	}
	session("gone-revoked", "120 days", "100 days", "100 days") // ended 100 days ago
	session("gone-old", "200 days", "1 day", "")                // expired at 30 days: 170 days ago
	session("gone-idle", "105 days", "105 days", "")            // idle 7 days: 98 days ago
	session("kept-revoked", "85 days", "80 days", "80 days")    // ended 80 days ago
	session("kept-idle", "90 days", "90 days", "")              // ended 83 days ago
	session("kept-live", "1 day", "1 hour", "")

	// Invites: only one never used and long expired goes.
	invite := func(email, created, expires string, used bool) {
		exec(t, db, `INSERT INTO invites (email, token_hash, created_at, expires_at, used_at, user_id)
			VALUES ($1, sha256(convert_to($1, 'UTF8')), now() - $2::interval, now() - $3::interval,
			        CASE WHEN $4 THEN now() - $3::interval - interval '1 hour' END, CASE WHEN $4 THEN $5::uuid END)`,
			email, created, expires, used, u.ID)
	}
	invite("gone@example.com", "100 days", "97 days", false)
	invite("kept-recent@example.com", "20 days", "17 days", false)
	invite("ana-old@example.com", "400 days", "397 days", true)

	// Audit events: only the personal fields of those over a year old go.
	event := func(age string) {
		exec(t, db, `INSERT INTO audit_events (at, actor, action, ip, user_agent, outcome, details)
			VALUES (now() - $1::interval, 'anonymous', 'auth.login', '203.0.113.7', 'ua', 'denied',
			        '{"email": "someone@example.com", "reason": "bad password"}')`, age)
	}
	event("13 months")
	event("11 months")

	p, err := identity.PurgePersonalData(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	if p != (identity.Purged{Sessions: 3, Invites: 1, AuditEvents: 1}) {
		t.Errorf("purged = %+v, want 3 sessions, 1 invite, 1 audit event", p)
	}
	if n := count(t, db, "SELECT count(*) FROM sessions WHERE user_agent LIKE 'gone%'"); n != 0 {
		t.Errorf("%d sessions past retention left", n)
	}
	if n := count(t, db, "SELECT count(*) FROM sessions WHERE user_agent LIKE 'kept%'"); n != 3 {
		t.Errorf("%d sessions within retention left, want 3", n)
	}
	if n := count(t, db, "SELECT count(*) FROM invites"); n != 2 {
		t.Errorf("%d invites left, want 2", n)
	}
	if n := count(t, db, `SELECT count(*) FROM audit_events WHERE at < now() - interval '12 months'
		AND ip IS NULL AND user_agent IS NULL AND details = '{"reason": "bad password"}'`); n != 1 {
		t.Errorf("the old event wasn't scrubbed to its reason (%d)", n)
	}
	if n := count(t, db, `SELECT count(*) FROM audit_events WHERE at > now() - interval '12 months'
		AND ip IS NOT NULL AND details ? 'email'`); n != 1 {
		t.Errorf("the recent event was changed (%d)", n)
	}

	// A second run finds nothing left to do.
	if p, err := identity.PurgePersonalData(ctx, api); err != nil || p.Any() {
		t.Errorf("second purge = %+v, %v; want nothing", p, err)
	}

	// The function is otter_api's only way in: it still can't delete a
	// session or edit an audit event directly.
	for _, sql := range []string{
		"DELETE FROM sessions",
		"UPDATE audit_events SET ip = NULL",
		"DELETE FROM invites",
	} {
		_, err := api.Exec(ctx, sql)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
			t.Errorf("%s as otter_api: err = %v, want permission denied", sql, err)
		}
	}
	// And otter_admin can't run the purge.
	if _, err := identity.PurgePersonalData(ctx, db.ConnAs(dbtest.RoleAdmin)); err == nil {
		t.Error("otter_admin ran the purge")
	}
}
