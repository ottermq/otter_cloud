package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultResetTTL is how long a password reset link stays valid (#160):
// shorter than an invite's, since it takes over an account that exists.
const DefaultResetTTL = time.Hour

// MaxResetTTL is the longest a link may live, for a hand-off the user
// won't see at once. The table checks it too.
const MaxResetTTL = 24 * time.Hour

// Errors a person resetting their password can act on. Their text is safe
// to show. As with invites, an unknown, used or replaced link gets the
// same error, and so does one whose user or account is no longer active:
// a link names no address, and shouldn't tell what became of one.
var (
	ErrResetInvalid = errors.New("this reset link is not valid or has already been used")
	ErrResetExpired = errors.New("this reset link has expired; ask the operator for a new one")
)

// ErrUserInactive means the user is disabled or their account isn't
// active: an operator re-enables it first, as its own audited step.
var ErrUserInactive = errors.New("the user is disabled or their account is not active; enable it first")

// Reset is a password reset link issued for a user.
type Reset struct {
	ID        string
	User      User
	CreatedAt time.Time
	ExpiresAt time.Time
	Replaced  int64 // earlier unused links this one revoked
}

// CreateReset issues a reset link for the user with email, valid for ttl,
// and returns it with its token, which is not stored and can't be shown
// again. Earlier unused links for the user are revoked. A disabled user,
// or one whose account is suspended or closed, is refused with
// ErrUserInactive. Run it in a transaction with the audit event.
func CreateReset(ctx context.Context, db DB, email string, ttl time.Duration) (Reset, string, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Reset{}, "", err
	}
	if ttl <= 0 || ttl > MaxResetTTL {
		return Reset{}, "", fmt.Errorf("reset link lifetime must be between 0 and %s, got %s", MaxResetTTL, ttl)
	}
	var r Reset
	u := &r.User
	var accountStatus string
	// Locked, so two links issued at once can't both survive.
	err = db.QueryRow(ctx,
		`SELECT u.id, u.account_id, u.email, u.status, u.is_operator, u.created_at, a.status
		 FROM users u JOIN accounts a ON a.id = u.account_id
		 WHERE u.email = $1 FOR UPDATE OF u`, email).
		Scan(&u.ID, &u.AccountID, &u.Email, &u.Status, &u.IsOperator, &u.CreatedAt, &accountStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reset{}, "", ErrUserNotFound
	}
	if err != nil {
		return Reset{}, "", fmt.Errorf("look up user: %w", err)
	}
	if u.Status != "active" || accountStatus != "active" {
		return Reset{}, "", ErrUserInactive
	}

	tag, err := db.Exec(ctx,
		`UPDATE password_resets SET revoked_at = now()
		 WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()`, u.ID)
	if err != nil {
		return Reset{}, "", fmt.Errorf("revoke earlier links: %w", err)
	}
	r.Replaced = tag.RowsAffected()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Reset{}, "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := db.QueryRow(ctx,
		`INSERT INTO password_resets (user_id, token_hash, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3))
		 RETURNING id, created_at, expires_at`,
		u.ID, hashToken(token), ttl.Seconds()).Scan(&r.ID, &r.CreatedAt, &r.ExpiresAt); err != nil {
		return Reset{}, "", fmt.Errorf("create reset link: %w", err)
	}
	return r, token, nil
}

// ResetLink is the reset page's link for token, in the fragment for the
// same reason as an invite's (InviteLink).
func ResetLink(publicURL *url.URL, token string) string {
	u := *publicURL
	u.Path = "/reset-password"
	u.Fragment = "token=" + token
	return u.String()
}

// UseReset consumes a reset link: the user's password becomes password,
// under the policy, and every session of theirs ends. The caller issues a
// new one for the request. The link is locked, so it can't be used twice.
func UseReset(ctx context.Context, db DB, token, password string) (User, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 128 {
		return User{}, ErrResetInvalid
	}
	sum := hashToken(token)

	// Check the link before hashing the password, as sign-up does.
	_, u, err := usableReset(ctx, db, sum, false)
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(password, u.Email); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(ctx, password)
	if err != nil {
		return User{}, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, u, err := usableReset(ctx, tx, sum, true)
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, u.ID, hash); err != nil {
		return User{}, fmt.Errorf("set password: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE password_resets SET used_at = now() WHERE id = $1`, id); err != nil {
		return User{}, fmt.Errorf("mark reset link used: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, u.ID); err != nil {
		return User{}, fmt.Errorf("end sessions: %w", err)
	}
	return u, tx.Commit(ctx)
}

func usableReset(ctx context.Context, q querier, tokenHash []byte, lock bool) (string, User, error) {
	sql := `SELECT r.id, r.used_at IS NOT NULL OR r.revoked_at IS NOT NULL, r.expires_at <= now(),
	               u.id, u.account_id, u.email, u.status, u.is_operator, u.created_at, a.status
	        FROM password_resets r
	        JOIN users u ON u.id = r.user_id
	        JOIN accounts a ON a.id = u.account_id
	        WHERE r.token_hash = $1`
	if lock {
		sql += " FOR UPDATE OF r, u"
	}
	var id, accountStatus string
	var spent, expired bool
	var u User
	err := q.QueryRow(ctx, sql, tokenHash).Scan(&id, &spent, &expired,
		&u.ID, &u.AccountID, &u.Email, &u.Status, &u.IsOperator, &u.CreatedAt, &accountStatus)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", User{}, ErrResetInvalid
	case err != nil:
		return "", User{}, fmt.Errorf("look up reset link: %w", err)
	case spent, u.Status != "active", accountStatus != "active":
		return "", User{}, ErrResetInvalid
	case expired:
		return "", User{}, ErrResetExpired
	}
	return id, u, nil
}
