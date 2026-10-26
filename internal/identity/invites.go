package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultInviteTTL is how long an invite stays valid (security.md §6).
const DefaultInviteTTL = 72 * time.Hour

const maxInviteTTL = 30 * 24 * time.Hour

// Errors a person signing up can act on. Their text is safe to show. An
// unknown token and a used one get the same error: the token is the only
// thing the caller has, and there's nothing different to do about either.
var (
	ErrInviteInvalid = errors.New("this invite link is not valid or has already been used")
	ErrInviteExpired = errors.New("this invite link has expired; ask for a new one")
)

// Invite is an invitation to sign up with one email address.
type Invite struct {
	ID        string
	Email     string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	UserID    *string // the user who signed up with it
}

// State is "used", "expired" or "pending" at time now.
func (i Invite) State(now time.Time) string {
	switch {
	case i.UsedAt != nil:
		return "used"
	case !now.Before(i.ExpiresAt):
		return "expired"
	default:
		return "pending"
	}
}

// CreateInvite creates an invite for email, valid for ttl, and returns it
// with its token. The token is not stored and can't be shown again. An
// address that already has a user is refused with ErrEmailTaken; earlier
// invites for the same address stay valid until used or expired.
func CreateInvite(ctx context.Context, db DB, email string, ttl time.Duration) (Invite, string, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Invite{}, "", err
	}
	if ttl <= 0 || ttl > maxInviteTTL {
		return Invite{}, "", fmt.Errorf("invite lifetime must be between 0 and %s, got %s", maxInviteTTL, ttl)
	}
	var taken bool
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT FROM users WHERE email = $1)", email).Scan(&taken); err != nil {
		return Invite{}, "", fmt.Errorf("look up user: %w", err)
	}
	if taken {
		return Invite{}, "", ErrEmailTaken
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Invite{}, "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	inv := Invite{Email: email}
	if err := db.QueryRow(ctx,
		`INSERT INTO invites (email, token_hash, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3))
		 RETURNING id, created_at, expires_at`,
		email, sum[:], ttl.Seconds()).Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
		return Invite{}, "", fmt.Errorf("create invite: %w", err)
	}
	return inv, token, nil
}

// InviteLink is the sign-up link for token. The token goes in the fragment,
// which browsers never send to a server, so it can't land in access logs,
// proxy logs or a Referer header; the sign-up page reads it from there.
func InviteLink(publicURL *url.URL, token string) string {
	u := *publicURL
	u.Path = "/signup"
	u.Fragment = "invite=" + token
	return u.String()
}

// ListInvites returns every invite, newest first.
func ListInvites(ctx context.Context, db DB) ([]Invite, error) {
	rows, err := db.Query(ctx,
		`SELECT id, email, created_at, expires_at, used_at, user_id
		 FROM invites ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invite, error) {
		var i Invite
		err := r.Scan(&i.ID, &i.Email, &i.CreatedAt, &i.ExpiresAt, &i.UsedAt, &i.UserID)
		return i, err
	})
}

// SignUp is a new user's registration with an invite.
type SignUp struct {
	Token       string
	Password    string
	AccountName string // defaults to the email address
}

// SignUpWithInvite consumes an invite and creates its user and account, in
// one transaction. The email is the invite's: a person can't sign up with
// another address than the one invited.
func SignUpWithInvite(ctx context.Context, db DB, s SignUp) (User, error) {
	token := strings.TrimSpace(s.Token)
	if token == "" || len(token) > 128 {
		return User{}, ErrInviteInvalid
	}
	sum := sha256.Sum256([]byte(token))

	// Check the invite before hashing the password: hashing is the expensive
	// part, and a bad link shouldn't cost it.
	inv, err := usableInvite(ctx, db, sum[:], false)
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(s.Password, inv.Email); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(ctx, s.Password)
	if err != nil {
		return User{}, err
	}
	name := strings.TrimSpace(s.AccountName)
	if name == "" {
		name = inv.Email
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Again, locked: two sign-ups racing on one link must not both succeed.
	if inv, err = usableInvite(ctx, tx, sum[:], true); err != nil {
		return User{}, err
	}
	u, err := insertUser(ctx, tx, inv.Email, hash, name, false)
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx,
		"UPDATE invites SET used_at = now(), user_id = $2 WHERE id = $1",
		inv.ID, u.ID); err != nil {
		return User{}, fmt.Errorf("mark invite used: %w", err)
	}
	return u, tx.Commit(ctx)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func usableInvite(ctx context.Context, q querier, tokenHash []byte, lock bool) (Invite, error) {
	sql := `SELECT id, email, used_at, expires_at <= now() FROM invites WHERE token_hash = $1`
	if lock {
		sql += " FOR UPDATE"
	}
	var inv Invite
	var expired bool
	err := q.QueryRow(ctx, sql, tokenHash).Scan(&inv.ID, &inv.Email, &inv.UsedAt, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Invite{}, ErrInviteInvalid
	case err != nil:
		return Invite{}, fmt.Errorf("look up invite: %w", err)
	case inv.UsedAt != nil:
		return Invite{}, ErrInviteInvalid
	case expired:
		return Invite{}, ErrInviteExpired
	}
	return inv, nil
}
