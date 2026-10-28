package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session lifetimes (security.md §6). A session ends at whichever comes
// first.
const (
	SessionIdleTimeout     = 7 * 24 * time.Hour
	SessionAbsoluteTimeout = 30 * 24 * time.Hour

	// lastSeenInterval limits how often a request writes last_seen_at: the
	// idle timeout is days, so minutes of slack cost nothing and save a
	// write on almost every request.
	lastSeenInterval = 5 * time.Minute
	maxUserAgent     = 512
)

// ErrLoginFailed covers an unknown address, a wrong password and a disabled
// user or account alike, so a login form can't be used to learn which
// addresses have accounts. Its text is safe to show.
var ErrLoginFailed = errors.New("email or password is incorrect")

// ErrNoSession means the token doesn't name a live session: unknown,
// revoked, expired, or its user or account no longer active.
var ErrNoSession = errors.New("not logged in")

// Session is a logged-in user's session.
type Session struct {
	ID        string
	User      User
	CreatedAt time.Time
}

// Client describes where a login came from, for the session's record.
type Client struct {
	IP        netip.Addr // zero if unknown
	UserAgent string
}

// Login checks an email and password and returns the user. Unknown
// addresses cost the same argon2id work as known ones, so response time
// doesn't reveal which exist. A hash with old parameters is replaced.
func Login(ctx context.Context, db DB, email, password string) (User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, ErrLoginFailed
	}
	var u User
	var hash, accountStatus string
	err = db.QueryRow(ctx,
		`SELECT u.id, u.account_id, u.email, u.status, u.is_operator, u.created_at, u.password_hash, a.status
		 FROM users u JOIN accounts a ON a.id = u.account_id
		 WHERE u.email = $1`, email).
		Scan(&u.ID, &u.AccountID, &u.Email, &u.Status, &u.IsOperator, &u.CreatedAt, &hash, &accountStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := VerifyPassword(ctx, password, dummyHash()); err != nil {
			return User{}, err
		}
		return User{}, ErrLoginFailed
	}
	if err != nil {
		return User{}, fmt.Errorf("look up user: %w", err)
	}
	ok, err := VerifyPassword(ctx, password, hash)
	if err != nil {
		return User{}, err
	}
	if !ok || u.Status != "active" || accountStatus != "active" {
		return User{}, ErrLoginFailed
	}
	if NeedsRehash(hash) {
		// Best effort: the login has succeeded either way.
		if fresh, err := HashPassword(ctx, password); err == nil {
			_, _ = db.Exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", u.ID, fresh)
		}
	}
	return u, nil
}

// dummyHash is a valid hash of a random password, made once, for Login to
// verify against when the address is unknown.
var dummyHash = sync.OnceValue(func() string {
	pw := make([]byte, 32)
	_, _ = rand.Read(pw)
	h, err := HashPassword(context.Background(), string(pw))
	if err != nil {
		panic("identity: hash the dummy password: " + err.Error())
	}
	return h
})

// CreateSession starts a session for userID and returns its token, which
// goes in the cookie and is not stored.
func CreateSession(ctx context.Context, db DB, userID string, c Client) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	var ip *netip.Addr
	if c.IP.IsValid() {
		ip = &c.IP
	}
	ua := strings.ToValidUTF8(c.UserAgent, "")
	if len(ua) > maxUserAgent {
		ua = strings.ToValidUTF8(ua[:maxUserAgent], "")
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO sessions (token_hash, user_id, created_ip, user_agent) VALUES ($1, $2, $3, $4)",
		hashToken(token), userID, ip, ua); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// Authenticate returns the live session that token names, and records that
// it was seen. The checks are in the query, so a disabled user or a closed
// account loses every session at once.
func Authenticate(ctx context.Context, db DB, token string) (Session, error) {
	if token == "" || len(token) > 128 {
		return Session{}, ErrNoSession
	}
	var s Session
	var lastSeen time.Time
	err := db.QueryRow(ctx,
		`SELECT s.id, s.created_at, s.last_seen_at,
		        u.id, u.account_id, u.email, u.status, u.is_operator, u.created_at
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 JOIN accounts a ON a.id = u.account_id
		 WHERE s.token_hash = $1
		   AND s.revoked_at IS NULL
		   AND s.created_at > now() - make_interval(secs => $2)
		   AND s.last_seen_at > now() - make_interval(secs => $3)
		   AND u.status = 'active' AND a.status = 'active'`,
		hashToken(token), SessionAbsoluteTimeout.Seconds(), SessionIdleTimeout.Seconds()).
		Scan(&s.ID, &s.CreatedAt, &lastSeen,
			&s.User.ID, &s.User.AccountID, &s.User.Email, &s.User.Status, &s.User.IsOperator, &s.User.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("look up session: %w", err)
	}
	if time.Since(lastSeen) > lastSeenInterval {
		if _, err := db.Exec(ctx, "UPDATE sessions SET last_seen_at = now() WHERE id = $1", s.ID); err != nil {
			return Session{}, fmt.Errorf("touch session: %w", err)
		}
	}
	return s, nil
}

// RevokeSession ends the session token names, if any, and returns whose it
// was (both empty when there was no live session). Revoking an unknown or
// already revoked token is not an error: logout is idempotent.
func RevokeSession(ctx context.Context, db DB, token string) (userID, accountID string, err error) {
	if token == "" || len(token) > 128 {
		return "", "", nil
	}
	err = db.QueryRow(ctx,
		`UPDATE sessions s SET revoked_at = now()
		 FROM users u
		 WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND u.id = s.user_id
		 RETURNING u.id, u.account_id`,
		hashToken(token)).Scan(&userID, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return userID, accountID, err
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
