package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrUserNotFound means no user has the address.
var ErrUserNotFound = errors.New("no user with this email address")

// StatusChange is what SetUserStatus did.
type StatusChange struct {
	User            User   // after the change
	Previous        string // the status before
	SessionsRevoked int64
}

// Changed reports whether the status actually changed.
func (c StatusChange) Changed() bool { return c.Previous != c.User.Status }

// SetUserStatus sets the status of the user with email to "active" or
// "disabled". Disabling also revokes every session the user has, so that
// enabling them again later doesn't bring old (possibly stolen) cookies
// back to life. Run it in a transaction with the audit event.
func SetUserStatus(ctx context.Context, db DB, email, status string) (StatusChange, error) {
	if status != "active" && status != "disabled" {
		return StatusChange{}, fmt.Errorf("unknown user status %q", status)
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return StatusChange{}, err
	}
	var c StatusChange
	u := &c.User
	err = db.QueryRow(ctx,
		`SELECT id, account_id, email, status, is_operator, created_at
		 FROM users WHERE email = $1 FOR UPDATE`, email).
		Scan(&u.ID, &u.AccountID, &u.Email, &c.Previous, &u.IsOperator, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusChange{}, ErrUserNotFound
	}
	if err != nil {
		return StatusChange{}, fmt.Errorf("look up user: %w", err)
	}
	u.Status = status
	if c.Previous != status {
		if _, err := db.Exec(ctx, "UPDATE users SET status = $2 WHERE id = $1", u.ID, status); err != nil {
			return StatusChange{}, fmt.Errorf("set user status: %w", err)
		}
	}
	if status == "disabled" {
		tag, err := db.Exec(ctx,
			"UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL", u.ID)
		if err != nil {
			return StatusChange{}, fmt.Errorf("revoke sessions: %w", err)
		}
		c.SessionsRevoked = tag.RowsAffected()
	}
	return c, nil
}

// UserSummary is a row of ListUsers.
type UserSummary struct {
	User
	AccountStatus  string
	ActiveSessions int
	LastSeen       *time.Time // the most recent session activity
}

// ListUsers returns every user, oldest first, with their live session count.
func ListUsers(ctx context.Context, db DB) ([]UserSummary, error) {
	rows, err := db.Query(ctx,
		`SELECT u.id, u.account_id, u.email, u.status, u.is_operator, u.created_at, a.status,
		        count(s.id) FILTER (WHERE s.revoked_at IS NULL
		                              AND s.created_at > now() - make_interval(secs => $1)
		                              AND s.last_seen_at > now() - make_interval(secs => $2)),
		        max(s.last_seen_at)
		 FROM users u
		 JOIN accounts a ON a.id = u.account_id
		 LEFT JOIN sessions s ON s.user_id = u.id
		 GROUP BY u.id, a.status
		 ORDER BY u.created_at, u.id`,
		SessionAbsoluteTimeout.Seconds(), SessionIdleTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserSummary, error) {
		var s UserSummary
		err := r.Scan(&s.ID, &s.AccountID, &s.Email, &s.Status, &s.IsOperator, &s.CreatedAt,
			&s.AccountStatus, &s.ActiveSessions, &s.LastSeen)
		return s, err
	})
}
