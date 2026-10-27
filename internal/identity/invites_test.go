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

const goodPassword = "correct horse battery staple"

func TestSignUpWithInvite(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)

	inv, token, err := identity.CreateInvite(ctx, api, " Ana@Example.com", identity.DefaultInviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "ana@example.com" || inv.State(time.Now()) != "pending" {
		t.Errorf("invite = %+v", inv)
	}
	if d := inv.ExpiresAt.Sub(inv.CreatedAt); d != identity.DefaultInviteTTL {
		t.Errorf("lifetime = %s, want %s", d, identity.DefaultInviteTTL)
	}
	var stored int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM invites WHERE position($1::bytea in token_hash) > 0", []byte(token)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Error("the token is stored in clear")
	}

	u, err := identity.SignUpWithInvite(ctx, api, identity.SignUp{Token: token, Password: goodPassword, AccountName: "Ana's Lab"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "ana@example.com" || u.IsOperator {
		t.Errorf("user = %+v", u)
	}
	var name string
	if err := api.QueryRow(ctx, "SELECT name FROM accounts WHERE id = $1", u.AccountID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ana's Lab" {
		t.Errorf("account name = %q", name)
	}

	list, err := identity.ListInvites(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State(time.Now()) != "used" || list[0].UserID == nil || *list[0].UserID != u.ID {
		t.Errorf("invites after sign-up = %+v", list)
	}

	// Reused.
	_, err = identity.SignUpWithInvite(ctx, api, identity.SignUp{Token: token, Password: "another fine passphrase"})
	if !errors.Is(err, identity.ErrInviteInvalid) {
		t.Errorf("reused invite: err = %v, want ErrInviteInvalid", err)
	}
	// And the address can't be invited again.
	if _, _, err := identity.CreateInvite(ctx, api, "ana@example.com", time.Hour); !errors.Is(err, identity.ErrEmailTaken) {
		t.Errorf("invite for an existing user: err = %v, want ErrEmailTaken", err)
	}
}

func TestSignUpWithExpiredInvite(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)

	inv, token, err := identity.CreateInvite(ctx, api, "ana@example.com", identity.DefaultInviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Exec(ctx,
		"UPDATE invites SET created_at = now() - interval '4 days', expires_at = now() - interval '1 second' WHERE id = $1",
		inv.ID); err != nil {
		t.Fatal(err)
	}
	_, err = identity.SignUpWithInvite(ctx, api, identity.SignUp{Token: token, Password: goodPassword})
	if !errors.Is(err, identity.ErrInviteExpired) {
		t.Errorf("err = %v, want ErrInviteExpired", err)
	}
	var users int
	if err := api.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("%d users after an expired invite", users)
	}
}

func TestSignUpRefusals(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	api := db.ConnAs(dbtest.RoleAPI)

	_, token, err := identity.CreateInvite(ctx, api, "ana@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		token, password string
		want            error
	}{
		{"unknown token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", goodPassword, identity.ErrInviteInvalid},
		{"empty token", "", goodPassword, identity.ErrInviteInvalid},
		{"huge token", strings.Repeat("A", 4096), goodPassword, identity.ErrInviteInvalid},
		{"weak password", token, "qwertyuiop123", identity.ErrPasswordCommon},
		{"password is the email", token, "ana@example.com", identity.ErrPasswordIsEmail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := identity.SignUpWithInvite(ctx, api, identity.SignUp{Token: tc.token, Password: tc.password})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// A refused password doesn't use up the invite.
	if _, err := identity.SignUpWithInvite(ctx, api, identity.SignUp{Token: token, Password: goodPassword}); err != nil {
		t.Errorf("sign-up after a refused password: %v", err)
	}
}

func TestSignUpRace(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	setup := db.ConnAs(dbtest.RoleAPI)

	_, token, err := identity.CreateInvite(ctx, setup, "ana@example.com", time.Hour)
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
		wg.Go(func() {
			_, errs[i] = identity.SignUpWithInvite(ctx, conns[i], identity.SignUp{Token: token, Password: goodPassword})
		})
	}
	wg.Wait()

	var ok int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, identity.ErrInviteInvalid):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d sign-ups succeeded with one invite, want 1 (errors: %v)", ok, errs)
	}
}

func TestCreateInviteRefusals(t *testing.T) {
	db := dbtest.New(t)
	api := db.ConnAs(dbtest.RoleAPI)
	ctx := context.Background()
	if _, _, err := identity.CreateInvite(ctx, api, "not an email", time.Hour); !errors.Is(err, identity.ErrInvalidEmail) {
		t.Errorf("bad email: err = %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Hour, 31 * 24 * time.Hour} {
		if _, _, err := identity.CreateInvite(ctx, api, "ana@example.com", ttl); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
}

func TestInviteLink(t *testing.T) {
	u, _ := url.Parse("https://cloud.example.com")
	if got, want := identity.InviteLink(u, "abc-_123"), "https://cloud.example.com/signup#invite=abc-_123"; got != want {
		t.Errorf("InviteLink = %q, want %q", got, want)
	}
}
