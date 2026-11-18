package instances_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

// count runs a counting query as otter_migrator.
func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestEraseUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	account, actor := f.customer("ana@example.com", true)
	_, otherActor := f.customer("bia@example.com", true)
	id := strings.TrimPrefix(actor, "user:")

	c, err := f.create(account, actor, "production", "db-small")
	if err != nil {
		t.Fatal(err)
	}
	f.migrate(`INSERT INTO sessions (token_hash, user_id, created_ip, user_agent)
		VALUES (sha256('a'), $1, '203.0.113.7', 'ua')`, id)
	f.migrate(`INSERT INTO password_resets (user_id, token_hash, expires_at)
		VALUES ($1, sha256('r'), now() + interval '1 hour')`, id)
	f.migrate(`INSERT INTO invites (email, token_hash, expires_at, used_at, user_id)
		VALUES ('ana@example.com', sha256('i'), now() + interval '1 day', now(), $1)`, id)
	// Her own login, a failed login typed with her address, and someone
	// else's event, which stays as it is.
	f.migrate(`INSERT INTO audit_events (actor, account_id, action, ip, user_agent, outcome)
		VALUES ($1, $2, 'auth.login', '203.0.113.7', 'ua', 'ok')`, actor, account)
	f.migrate(`INSERT INTO audit_events (actor, action, ip, outcome, details)
		VALUES ('anonymous', 'auth.login', '198.51.100.9', 'denied', '{"email": "ana@example.com"}')`)
	f.migrate(`INSERT INTO audit_events (actor, action, ip, outcome, details)
		VALUES ($1, 'auth.login', '192.0.2.1', 'ok', '{"email": "bia@example.com"}')`, otherActor)

	if _, err := identity.EraseUser(ctx, admin, "ana@example.com"); !errors.Is(err, identity.ErrHasDatabases) {
		t.Fatalf("erase with a live database: err = %v, want ErrHasDatabases", err)
	}
	f.migrate(`UPDATE instances SET status = 'deleted', desired_state = 'deleted', deleted_at = now() WHERE id = $1`, c.Instance.ID)

	e, err := identity.EraseUser(ctx, admin, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if e.Sessions != 1 || e.AuditScrubbed < 2 {
		t.Errorf("erasure = %+v, want 1 session and at least 2 events", e)
	}

	// Nothing left names her.
	for _, q := range []string{
		"SELECT count(*) FROM users WHERE email = 'ana@example.com'",
		"SELECT count(*) FROM accounts WHERE name = 'ana@example.com'",
		"SELECT count(*) FROM invites WHERE email = 'ana@example.com'",
		"SELECT count(*) FROM audit_events WHERE details::text LIKE '%ana@example.com%'",
		"SELECT count(*) FROM audit_events WHERE ip IN ('203.0.113.7', '198.51.100.9') OR user_agent IS NOT NULL",
	} {
		if n := f.count(q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	if n := f.count("SELECT count(*) FROM sessions WHERE user_id = $1", id); n != 0 {
		t.Errorf("%d sessions left", n)
	}
	if n := f.count("SELECT count(*) FROM password_resets WHERE user_id = $1", id); n != 0 {
		t.Errorf("%d reset links left", n)
	}
	if n := f.count(`SELECT count(*) FROM users u JOIN accounts a ON a.id = u.account_id
		WHERE u.id = $1 AND u.status = 'disabled' AND a.status = 'closed' AND u.email LIKE 'erased-%@invalid'`, id); n != 1 {
		t.Error("the user isn't disabled with a tombstone address, or the account isn't closed")
	}
	// The events stay, by user id.
	if n := f.count("SELECT count(*) FROM audit_events WHERE actor = $1", actor); n != 1 {
		t.Errorf("her audit events = %d, want 1 kept", n)
	}
	// Bia's event is untouched.
	if n := f.count(`SELECT count(*) FROM audit_events WHERE ip = '192.0.2.1' AND details->>'email' = 'bia@example.com'`); n != 1 {
		t.Error("another user's event was changed")
	}
	// Her password no longer logs in, and her address is free again.
	if _, err := identity.Login(ctx, f.db.ConnAs(dbtest.RoleAPI), "ana@example.com", "correct horse battery staple"); err == nil {
		t.Error("the erased user logged in")
	}
	if _, err := identity.EraseUser(ctx, admin, "ana@example.com"); !errors.Is(err, identity.ErrUserNotFound) {
		t.Errorf("second erase: err = %v, want ErrUserNotFound", err)
	}
	// otter_api can't erase anyone.
	if _, err := f.db.ConnAs(dbtest.RoleAPI).Exec(ctx, "SELECT erase_user($1)", id); err == nil {
		t.Error("otter_api ran erase_user")
	}
}

func TestEraseRefusesAnOperator(t *testing.T) {
	f := newFixture(t)
	_, actor := f.customer("op@example.com", false)
	f.migrate("UPDATE users SET is_operator = true WHERE id = $1", strings.TrimPrefix(actor, "user:"))
	_, err := identity.EraseUser(context.Background(), f.db.ConnAs(dbtest.RoleAdmin), "op@example.com")
	if !errors.Is(err, identity.ErrOperator) {
		t.Errorf("err = %v, want ErrOperator", err)
	}
}
