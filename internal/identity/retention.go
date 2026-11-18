package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultPurgeEvery is how often the api purges personal data past its
// retention (#219).
const DefaultPurgeEvery = time.Hour

// Purged is what one PurgePersonalData removed.
type Purged struct {
	Sessions, Invites, AuditEvents int64
}

// Any reports whether the purge changed anything.
func (p Purged) Any() bool { return p.Sessions+p.Invites+p.AuditEvents > 0 }

// PurgePersonalData applies the privacy notice's retention
// (internal/legal/texts/privacidade.md §5): sessions 90 days after they ended,
// invites never used 90 days after they expired, and IPs, user agents and
// emails in audit events once the event is 12 months old. The rules are in
// purge_personal_data (migration 0025), the only way otter_api can delete
// or edit those rows.
func PurgePersonalData(ctx context.Context, db DB) (Purged, error) {
	var p Purged
	err := db.QueryRow(ctx, "SELECT * FROM purge_personal_data()").
		Scan(&p.Sessions, &p.Invites, &p.AuditEvents)
	if err != nil {
		return Purged{}, fmt.Errorf("purge personal data: %w", err)
	}
	return p, nil
}

// RunPurge purges at once and then every every (DefaultPurgeEvery if 0)
// until ctx ends. audit records what each purge that changed anything
// removed, as counts.
func RunPurge(ctx context.Context, db DB, log *slog.Logger, every time.Duration, audit func(context.Context, Purged) error) {
	if every <= 0 {
		every = DefaultPurgeEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		p, err := PurgePersonalData(ctx, db)
		if err == nil && p.Any() {
			log.Info("retention: personal data purged",
				"sessions", p.Sessions, "invites", p.Invites, "audit_events", p.AuditEvents)
			if audit != nil {
				err = audit(ctx, p)
			}
		}
		if err != nil && ctx.Err() == nil {
			log.Error("retention", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ErrHasDatabases means the account still has databases that are not
// deleted; they go first, through the normal delete.
var ErrHasDatabases = errors.New("the account still has databases that are not deleted: delete them first")

// ErrOperator means the user is an operator, whom erasure refuses.
var ErrOperator = errors.New("the user is an operator")

// Erasure is what EraseUser did.
type Erasure struct {
	User                    User // before the erasure: the caller's to report, never to store
	Sessions, AuditScrubbed int64
}

// EraseUser erases the personal data of the user with email, on their
// request (the privacy notice's §5 and §7): see erase_user in migration
// 0025. It can't be undone. Run it in a transaction with the audit event,
// which must not carry the email.
func EraseUser(ctx context.Context, db DB, email string) (Erasure, error) {
	u, err := UserByEmail(ctx, db, email)
	if err != nil {
		return Erasure{}, err
	}
	e := Erasure{User: u}
	err = db.QueryRow(ctx, "SELECT * FROM erase_user($1)", u.ID).Scan(&e.Sessions, &e.AuditScrubbed)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "55000":
			return Erasure{}, ErrHasDatabases
		case "42501":
			if u.IsOperator {
				return Erasure{}, ErrOperator
			}
		case "P0002":
			return Erasure{}, ErrUserNotFound
		}
	}
	if err != nil {
		return Erasure{}, fmt.Errorf("erase user: %w", err)
	}
	return e, nil
}
