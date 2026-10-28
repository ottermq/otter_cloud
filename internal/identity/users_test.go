package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

func TestCreateUser(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)

	u, err := identity.CreateUser(ctx, api, identity.NewUser{
		Email:    " Ana@Example.com ",
		Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "ana@example.com" || u.Status != "active" || u.IsOperator || u.ID == "" || u.AccountID == "" {
		t.Errorf("user = %+v", u)
	}

	var name, hash string
	if err := api.QueryRow(ctx,
		"SELECT a.name, u.password_hash FROM users u JOIN accounts a ON a.id = u.account_id WHERE u.id = $1",
		u.ID).Scan(&name, &hash); err != nil {
		t.Fatal(err)
	}
	if name != "ana@example.com" {
		t.Errorf("account name = %q, want the email", name)
	}
	if ok, err := identity.VerifyPassword(ctx, "correct horse battery staple", hash); err != nil || !ok {
		t.Errorf("stored hash does not verify: ok=%v err=%v", ok, err)
	}

	_, err = identity.CreateUser(ctx, api, identity.NewUser{
		Email:    "ANA@example.com",
		Password: "another fine passphrase",
	})
	if !errors.Is(err, identity.ErrEmailTaken) {
		t.Errorf("duplicate email: err = %v, want ErrEmailTaken", err)
	}
	var accounts int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 {
		t.Errorf("%d accounts after a refused duplicate, want 1: the transaction leaked", accounts)
	}
}

func TestCreateUserRefusesBadInput(t *testing.T) {
	db := dbtest.New(t)
	api := db.ConnAs(dbtest.RoleAPI)
	for _, tc := range []struct {
		email, password string
		want            error
	}{
		{"not-an-email", "correct horse battery staple", identity.ErrInvalidEmail},
		{"ana@example.com", "short", identity.ErrPasswordTooShort},
		{"ana@example.com", "qwertyuiop123", identity.ErrPasswordCommon},
		{"ana@example.com", "ana@example.com", identity.ErrPasswordIsEmail},
	} {
		_, err := identity.CreateUser(context.Background(), api, identity.NewUser{Email: tc.email, Password: tc.password})
		if !errors.Is(err, tc.want) {
			t.Errorf("CreateUser(%q, %q) = %v, want %v", tc.email, tc.password, err, tc.want)
		}
	}
}

func TestIdentityPrivileges(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	worker := db.ConnAs(dbtest.RoleWorker)

	if _, err := identity.CreateUser(ctx, api, identity.NewUser{
		Email: "ana@example.com", Password: "correct horse battery staple",
	}); err != nil {
		t.Fatal(err)
	}

	denied := func(name string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: err = %v, want permission denied", name, err)
		}
	}
	_, err := api.Exec(ctx, "DELETE FROM users")
	denied("api deletes users", err)
	_, err = api.Exec(ctx, "DELETE FROM accounts")
	denied("api deletes accounts", err)
	_, err = worker.Exec(ctx, "SELECT FROM users")
	denied("worker reads users", err)
	_, err = worker.Exec(ctx, "SELECT FROM accounts")
	denied("worker reads accounts", err)

	_, err = api.Exec(ctx, "INSERT INTO users (account_id, email, password_hash) SELECT id, 'Bob@example.com', '$argon2id$x' FROM accounts")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("mixed-case email: err = %v, want a check violation", err)
	}
}

// otter_admin (Q16) may change a user's status and revoke sessions, and
// nothing else.
func TestAdminPrivileges(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	admin := db.ConnAs(dbtest.RoleAdmin)
	if _, err := identity.CreateUser(ctx, api, identity.NewUser{
		Email: "ana@example.com", Password: "correct horse battery staple",
	}); err != nil {
		t.Fatal(err)
	}

	for _, sql := range []string{
		"UPDATE users SET status = 'disabled'",
		"UPDATE sessions SET revoked_at = now()",
		"SELECT FROM accounts",
		"SELECT FROM invites",
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Errorf("admin: %s: %v", sql, err)
		}
	}
	for _, sql := range []string{
		"UPDATE users SET password_hash = '$argon2id$x'",
		"UPDATE users SET email = 'evil@example.com'",
		"UPDATE users SET is_operator = true",
		"UPDATE accounts SET status = 'closed'",
		"DELETE FROM users",
		"DELETE FROM invites",
		"UPDATE audit_events SET outcome = 'ok'",
		"DELETE FROM audit_events",
		"CREATE TABLE evil (id int)",
	} {
		_, err := admin.Exec(ctx, sql)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
			t.Errorf("admin: %s: err = %v, want permission denied", sql, err)
		}
	}
}
