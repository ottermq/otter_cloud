package identity_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

const newPassword = "a brand new passphrase"

// The acceptance of #160: a link works once; after it, old cookies and the
// old password are refused; the token isn't in the database. Issued as
// otter_admin and used as otter_api, the roles that do it in production.
func TestPasswordReset(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	admin := db.ConnAs(dbtest.RoleAdmin)

	u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: goodPassword})
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := identity.CreateSession(ctx, api, u.ID, identity.Client{})
	if err != nil {
		t.Fatal(err)
	}

	r, token, err := identity.CreateReset(ctx, admin, " Ana@Example.com", identity.DefaultResetTTL)
	if err != nil {
		t.Fatal(err)
	}
	if r.User.ID != u.ID || r.Replaced != 0 {
		t.Errorf("reset = %+v", r)
	}
	if d := r.ExpiresAt.Sub(r.CreatedAt); d != time.Hour {
		t.Errorf("lifetime = %s, want 1h", d)
	}
	var stored int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM password_resets WHERE position($1::bytea in token_hash) > 0", []byte(token)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Error("the token is stored in clear")
	}
	// Issuing a link changes nothing yet: the user may not be the one who
	// lost access.
	if _, err := identity.Authenticate(ctx, api, cookie); err != nil {
		t.Errorf("session after issuing a link: %v", err)
	}

	got, err := identity.UseReset(ctx, api, token, newPassword)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID {
		t.Errorf("user = %+v", got)
	}
	if _, err := identity.Authenticate(ctx, api, cookie); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("old session after a reset: %v, want ErrNoSession", err)
	}
	if _, err := identity.Login(ctx, api, "ana@example.com", goodPassword); !errors.Is(err, identity.ErrLoginFailed) {
		t.Errorf("old password after a reset: %v, want ErrLoginFailed", err)
	}
	if _, err := identity.Login(ctx, api, "ana@example.com", newPassword); err != nil {
		t.Errorf("new password: %v", err)
	}
	if _, err := identity.UseReset(ctx, api, token, "yet another passphrase"); !errors.Is(err, identity.ErrResetInvalid) {
		t.Errorf("reused link: %v, want ErrResetInvalid", err)
	}
}

// A newer link replaces an unused one.
func TestPasswordResetReplaced(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	admin := db.ConnAs(dbtest.RoleAdmin)
	if _, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	_, first, err := identity.CreateReset(ctx, admin, "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r, second, err := identity.CreateReset(ctx, admin, "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.Replaced != 1 {
		t.Errorf("replaced = %d, want 1", r.Replaced)
	}
	if _, err := identity.UseReset(ctx, api, first, newPassword); !errors.Is(err, identity.ErrResetInvalid) {
		t.Errorf("replaced link: %v, want ErrResetInvalid", err)
	}
	if _, err := identity.UseReset(ctx, api, second, newPassword); err != nil {
		t.Errorf("newer link: %v", err)
	}
}

func TestPasswordResetExpired(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	if _, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	r, token, err := identity.CreateReset(ctx, db.ConnAs(dbtest.RoleAdmin), "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConnAs(dbtest.RoleMigrator).Exec(ctx,
		"UPDATE password_resets SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1",
		r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.UseReset(ctx, api, token, newPassword); !errors.Is(err, identity.ErrResetExpired) {
		t.Errorf("err = %v, want ErrResetExpired", err)
	}
	if _, err := identity.Login(ctx, api, "ana@example.com", goodPassword); err != nil {
		t.Errorf("old password after an expired link: %v", err)
	}
}

// A disabled user, or one whose account is suspended, gets no link, and a
// link issued before is dead: re-enabling is its own step (#160).
func TestPasswordResetInactive(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	admin := db.ConnAs(dbtest.RoleAdmin)
	for _, email := range []string{"bob@example.com", "cid@example.com"} {
		if _, err := identity.CreateUser(ctx, api, identity.NewUser{Email: email, Password: goodPassword}); err != nil {
			t.Fatal(err)
		}
	}
	_, bobToken, err := identity.CreateReset(ctx, admin, "bob@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, cidToken, err := identity.CreateReset(ctx, admin, "cid@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Exec(ctx, "UPDATE users SET status = 'disabled' WHERE email = 'bob@example.com'"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Exec(ctx, "UPDATE accounts SET status = 'suspended' WHERE id = (SELECT account_id FROM users WHERE email = 'cid@example.com')"); err != nil {
		t.Fatal(err)
	}
	for email, token := range map[string]string{"bob@example.com": bobToken, "cid@example.com": cidToken} {
		if _, _, err := identity.CreateReset(ctx, admin, email, time.Hour); !errors.Is(err, identity.ErrUserInactive) {
			t.Errorf("%s: CreateReset = %v, want ErrUserInactive", email, err)
		}
		if _, err := identity.UseReset(ctx, api, token, newPassword); !errors.Is(err, identity.ErrResetInvalid) {
			t.Errorf("%s: UseReset = %v, want ErrResetInvalid", email, err)
		}
	}
}

func TestPasswordResetRefusals(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	admin := db.ConnAs(dbtest.RoleAdmin)
	if _, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := identity.CreateReset(ctx, admin, "zed@example.com", time.Hour); !errors.Is(err, identity.ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Hour, 25 * time.Hour} {
		if _, _, err := identity.CreateReset(ctx, admin, "ana@example.com", ttl); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	_, token, err := identity.CreateReset(ctx, admin, "ana@example.com", identity.MaxResetTTL)
	if err != nil {
		t.Fatalf("the longest ttl: %v", err)
	}
	for _, tc := range []struct {
		name            string
		token, password string
		want            error
	}{
		{"unknown token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", newPassword, identity.ErrResetInvalid},
		{"empty token", "", newPassword, identity.ErrResetInvalid},
		{"huge token", strings.Repeat("A", 4096), newPassword, identity.ErrResetInvalid},
		{"weak password", token, "qwertyuiop123", identity.ErrPasswordCommon},
		{"password is the email", token, "ana@example.com", identity.ErrPasswordIsEmail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := identity.UseReset(ctx, api, tc.token, tc.password); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// A refused password doesn't use up the link.
	if _, err := identity.UseReset(ctx, api, token, newPassword); err != nil {
		t.Errorf("reset after a refused password: %v", err)
	}
}

func TestPasswordResetRace(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	if _, err := identity.CreateUser(ctx, db.ConnAs(dbtest.RoleAPI), identity.NewUser{Email: "ana@example.com", Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	_, token, err := identity.CreateReset(ctx, db.ConnAs(dbtest.RoleAdmin), "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const n = 3
	conns := make([]identity.DB, n)
	for i := range conns {
		conns[i] = db.ConnAs(dbtest.RoleAPI)
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, errs[i] = identity.UseReset(ctx, conns[i], token, newPassword) })
	}
	wg.Wait()
	var ok int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, identity.ErrResetInvalid):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d resets succeeded with one link, want 1 (errors: %v)", ok, errs)
	}
}

func TestResetLink(t *testing.T) {
	u, _ := url.Parse("https://cloud.example.com")
	if got, want := identity.ResetLink(u, "abc-_123"), "https://cloud.example.com/reset-password#token=abc-_123"; got != want {
		t.Errorf("ResetLink = %q, want %q", got, want)
	}
}
