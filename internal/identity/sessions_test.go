package identity_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

func newUser(t *testing.T, db identity.DB, email string) identity.User {
	t.Helper()
	u, err := identity.CreateUser(context.Background(), db, identity.NewUser{Email: email, Password: goodPassword})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLogin(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	u := newUser(t, api, "ana@example.com")
	newUser(t, api, "bob@example.com")
	newUser(t, api, "cid@example.com")
	if _, err := api.Exec(ctx, "UPDATE users SET status = 'disabled' WHERE email = 'bob@example.com'"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Exec(ctx, "UPDATE accounts SET status = 'suspended' WHERE id = (SELECT account_id FROM users WHERE email = 'cid@example.com')"); err != nil {
		t.Fatal(err)
	}

	got, err := identity.Login(ctx, api, " ANA@example.com", goodPassword)
	if err != nil || got.ID != u.ID {
		t.Fatalf("Login = %+v, %v", got, err)
	}
	for _, tc := range []struct{ name, email, password string }{
		{"wrong password", "ana@example.com", "correct horse battery stapler"},
		{"unknown address", "zed@example.com", goodPassword},
		{"not an address", "zed", goodPassword},
		{"disabled user", "bob@example.com", goodPassword},
		{"suspended account", "cid@example.com", goodPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := identity.Login(ctx, api, tc.email, tc.password); !errors.Is(err, identity.ErrLoginFailed) {
				t.Errorf("err = %v, want ErrLoginFailed", err)
			}
		})
	}
}

func TestLoginRehashesOldHashes(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	u := newUser(t, api, "ana@example.com")
	old := identity.HashForTest(t, goodPassword, 8*1024, 1, 1)
	if _, err := api.Exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", u.ID, old); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Login(ctx, api, "ana@example.com", goodPassword); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := api.QueryRow(ctx, "SELECT password_hash FROM users WHERE id = $1", u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == old || identity.NeedsRehash(stored) {
		t.Errorf("hash not replaced after login: %s", stored)
	}
}

func TestSessions(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	u := newUser(t, api, "ana@example.com")

	client := identity.Client{IP: netip.MustParseAddr("192.0.2.7"), UserAgent: strings.Repeat("é", 400)}
	token, err := identity.CreateSession(ctx, api, u.ID, client)
	if err != nil {
		t.Fatal(err)
	}
	var clear int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE position($1::bytea in token_hash) > 0", []byte(token)).Scan(&clear); err != nil {
		t.Fatal(err)
	}
	if clear != 0 {
		t.Error("the token is stored in clear")
	}
	var ip, ua string
	if err := api.QueryRow(ctx, "SELECT host(created_ip), user_agent FROM sessions").Scan(&ip, &ua); err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.7" || len(ua) > 512 {
		t.Errorf("recorded ip %q, user agent of %d bytes", ip, len(ua))
	}

	s, err := identity.Authenticate(ctx, api, token)
	if err != nil || s.User.ID != u.ID || s.User.Email != "ana@example.com" {
		t.Fatalf("Authenticate = %+v, %v", s, err)
	}
	for _, bad := range []string{"", "nope", strings.Repeat("A", 4096), token + "x"} {
		if _, err := identity.Authenticate(ctx, api, bad); !errors.Is(err, identity.ErrNoSession) {
			t.Errorf("Authenticate(%.10q…) = %v, want ErrNoSession", bad, err)
		}
	}

	if err := identity.RevokeSession(ctx, api, token); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Authenticate(ctx, api, token); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("after revoke: %v, want ErrNoSession", err)
	}
	if err := identity.RevokeSession(ctx, api, token); err != nil {
		t.Errorf("second revoke: %v", err)
	}
}

func TestSessionEnds(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"idle timeout", "UPDATE sessions SET last_seen_at = now() - interval '7 days 1 minute'"},
		{"absolute timeout", "UPDATE sessions SET created_at = now() - interval '30 days 1 minute'"},
		{"user disabled", "UPDATE users SET status = 'disabled'"},
		{"account closed", "UPDATE accounts SET status = 'closed'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := dbtest.New(t)
			ctx := context.Background()
			api := db.ConnAs(dbtest.RoleAPI)
			u := newUser(t, api, "ana@example.com")
			token, err := identity.CreateSession(ctx, api, u.ID, identity.Client{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := api.Exec(ctx, tc.change); err != nil {
				t.Fatal(err)
			}
			if _, err := identity.Authenticate(ctx, api, token); !errors.Is(err, identity.ErrNoSession) {
				t.Errorf("err = %v, want ErrNoSession", err)
			}
		})
	}
}

func TestAuthenticateTouchesLastSeen(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)
	u := newUser(t, api, "ana@example.com")
	token, err := identity.CreateSession(ctx, api, u.ID, identity.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Exec(ctx, "UPDATE sessions SET last_seen_at = now() - interval '6 days'"); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Authenticate(ctx, api, token); err != nil {
		t.Fatal(err)
	}
	var recent bool
	if err := api.QueryRow(ctx, "SELECT last_seen_at > now() - interval '1 minute' FROM sessions").Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if !recent {
		t.Error("last_seen_at not updated: an active session would still expire after 7 days")
	}
}
