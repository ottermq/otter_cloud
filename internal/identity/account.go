package identity

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrWrongPassword means the current password given to change it is
// wrong. Its text is safe to show; it counts against the login limits.
var ErrWrongPassword = errors.New("the current password is incorrect")

// ErrSessionNotFound means the id names no live session of the user's:
// another user's looks the same.
var ErrSessionNotFound = errors.New("no such session")

// ChangePassword sets userID's password to next, after checking current
// and the password policy, and ends every one of the user's sessions: the
// caller issues a new one for the request's, so the change rotates it
// (#78). The user's row is locked, so two changes can't both pass the
// check against the same hash.
func ChangePassword(ctx context.Context, db DB, userID, current, next string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var email, hash string
	err = tx.QueryRow(ctx, `SELECT email, password_hash FROM users WHERE id = $1 AND status = 'active' FOR UPDATE`, userID).
		Scan(&email, &hash)
	if err != nil {
		return fmt.Errorf("look up user: %w", err)
	}
	ok, err := VerifyPassword(ctx, current, hash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrWrongPassword
	}
	if err := CheckPassword(next, email); err != nil {
		return err
	}
	fresh, err := HashPassword(ctx, next)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, fresh); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	return tx.Commit(ctx)
}

// SessionInfo is one of a user's live sessions, as the user sees it.
type SessionInfo struct {
	ID         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	IP         *netip.Addr // the address it logged in from, if known
	UserAgent  string
}

// uuidPattern is a session id's shape: anything else names no session,
// and isn't sent to PostgreSQL to fail as a uuid.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// liveSession is Authenticate's test of a live session, for a query that
// calls the table s.
const liveSession = `s.revoked_at IS NULL
	AND s.created_at > now() - make_interval(secs => $2)
	AND s.last_seen_at > now() - make_interval(secs => $3)`

// ListSessions returns userID's live sessions, newest first.
func ListSessions(ctx context.Context, db DB, userID string) ([]SessionInfo, error) {
	rows, err := db.Query(ctx, `
		SELECT s.id, s.created_at, s.last_seen_at, s.created_ip, coalesce(s.user_agent, '')
		FROM sessions s WHERE s.user_id = $1 AND `+liveSession+`
		ORDER BY s.created_at DESC`,
		userID, SessionAbsoluteTimeout.Seconds(), SessionIdleTimeout.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SessionInfo, error) {
		var s SessionInfo
		return s, r.Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt, &s.IP, &s.UserAgent)
	})
}

// EndSession ends userID's live session id. Another user's session, or
// one already over, is ErrSessionNotFound.
func EndSession(ctx context.Context, db DB, userID, id string) error {
	if !uuidPattern.MatchString(id) {
		return ErrSessionNotFound
	}
	tag, err := db.Exec(ctx, `UPDATE sessions s SET revoked_at = now() WHERE s.id = $4 AND s.user_id = $1 AND `+liveSession,
		userID, SessionAbsoluteTimeout.Seconds(), SessionIdleTimeout.Seconds(), id)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// EndOtherSessions ends every live session of userID's but keep, and says
// how many it ended.
func EndOtherSessions(ctx context.Context, db DB, userID, keep string) (int, error) {
	tag, err := db.Exec(ctx, `UPDATE sessions s SET revoked_at = now() WHERE s.user_id = $1 AND s.id <> $4 AND `+liveSession,
		userID, SessionAbsoluteTimeout.Seconds(), SessionIdleTimeout.Seconds(), keep)
	if err != nil {
		return 0, fmt.Errorf("end sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
