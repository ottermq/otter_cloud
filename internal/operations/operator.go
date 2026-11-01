package operations

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What operators do to the queue (`otter admin ops`, #102). The worker
// never calls these; otter_admin's grants allow nothing else (0015).

var (
	// ErrNotFound means no operation has that id.
	ErrNotFound = errors.New("no operation with that id")
	// ErrInFlight means the instance has another operation pending or
	// running, so this one can't join the queue.
	ErrInFlight = errors.New("the instance has another operation pending or running")
)

// Summary is an operation as an operator sees it, last_error included:
// never show one to a customer (portals.md §3).
type Summary struct {
	ID           string
	InstanceID   string
	InstanceName string
	Node         string
	Kind         string
	Status       string
	Step         string
	Attempts     int
	Reason       string
	LastError    string
	CreatedAt    time.Time
	FinishedAt   *time.Time
}

// Filter narrows List. Zero values match everything.
type Filter struct {
	Status     string
	InstanceID string
	Limit      int // 50 when zero
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// List returns operations, newest first.
func List(ctx context.Context, db DB, f Filter) ([]Summary, error) {
	if f.InstanceID != "" && !uuidPattern.MatchString(f.InstanceID) {
		return nil, fmt.Errorf("%q is not an instance id", f.InstanceID)
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := db.Query(ctx, summarySQL+`
		WHERE ($1 = '' OR o.status = $1) AND ($2 = '' OR o.instance_id::text = $2)
		ORDER BY o.created_at DESC, o.id DESC LIMIT $3`, f.Status, f.InstanceID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSummary)
}

const summarySQL = `
	SELECT o.id, o.instance_id, i.name, n.name, o.kind, o.status, coalesce(o.step, ''), o.attempts,
		coalesce(o.reason, ''), coalesce(o.last_error, ''), o.created_at, o.finished_at
	FROM operations o JOIN instances i ON i.id = o.instance_id JOIN infrastructure_nodes n ON n.id = o.node_id`

func scanSummary(row pgx.CollectableRow) (Summary, error) {
	var s Summary
	err := row.Scan(&s.ID, &s.InstanceID, &s.InstanceName, &s.Node, &s.Kind, &s.Status, &s.Step, &s.Attempts,
		&s.Reason, &s.LastError, &s.CreatedAt, &s.FinishedAt)
	return s, err
}

func summary(ctx context.Context, db DB, id string) (Summary, error) {
	rows, err := db.Query(ctx, summarySQL+` WHERE o.id = $1`, id)
	if err != nil {
		return Summary{}, err
	}
	s, err := pgx.CollectExactlyOneRow(rows, scanSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	return s, err
}

// locked is an operation and its instance, both locked FOR UPDATE: a
// worker's claim skips a locked row, so nothing claims it meanwhile.
type locked struct {
	kind, status, instanceID, instanceStatus, account string
	attempts                                          int
	createdAt                                         time.Time
}

func lock(ctx context.Context, tx DB, id string) (locked, error) {
	if !uuidPattern.MatchString(id) {
		return locked{}, ErrNotFound
	}
	var l locked
	err := tx.QueryRow(ctx, `
		SELECT o.kind, o.status, o.attempts, o.instance_id, i.status, o.account_id, o.created_at
		FROM operations o JOIN instances i ON i.id = o.instance_id
		WHERE o.id = $1 FOR UPDATE OF o, i`, id).
		Scan(&l.kind, &l.status, &l.attempts, &l.instanceID, &l.instanceStatus, &l.account, &l.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return locked{}, ErrNotFound
	}
	return l, err
}

// Change is what RetryFailed or CancelPending did, for the audit event.
type Change struct {
	Summary
	AccountID string
}

// RetryFailed puts a failed operation back in the queue, due now, with its
// attempts counted from zero again. It resumes from its last checkpoint
// (step), and last_error stays until its next run. The instance loses the
// operation's reason, and a delete's is deleting again. One that a later
// operation on the instance superseded is refused: a stop retried after
// the customer started the database again would undo that.
//
// A failed create can't be retried: failing cleared its payload, the
// password's SCRAM verifier, so a rerun has no password to set. The
// customer deletes the instance and creates another (#102). A failed
// reset_password, likewise: the customer asks for another. Run it in a
// transaction with the audit event.
func RetryFailed(ctx context.Context, tx DB, id string) (Change, error) {
	l, err := lock(ctx, tx, id)
	if err != nil {
		return Change{}, err
	}
	switch {
	case l.status != "failed":
		return Change{}, fmt.Errorf("only a failed operation can be retried; this one is %s", l.status)
	case l.kind == "create":
		return Change{}, errors.New("a failed create can't be retried: its password verifier is gone. " +
			"The customer deletes the database and creates a new one")
	case l.kind == "reset_password":
		return Change{}, errors.New("a failed password reset can't be retried: its password verifier is gone. " +
			"The customer asks for another reset")
	case l.instanceStatus == "deleted":
		return Change{}, errors.New("its instance is deleted; there is nothing left to retry")
	}
	// One later and still in flight is ErrInFlight, from the index below.
	var later bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM operations WHERE instance_id = $1 AND id <> $2 AND created_at > $3
		AND status NOT IN ('pending', 'running'))`,
		l.instanceID, id, l.createdAt).Scan(&later); err != nil {
		return Change{}, fmt.Errorf("look for a later operation: %w", err)
	}
	if later {
		return Change{}, errors.New("a later operation on its instance superseded it; retry that one, or ask the customer again")
	}
	_, err = tx.Exec(ctx, `
		UPDATE operations SET status = 'pending', attempts = 0, next_attempt_at = now(), locked_by = NULL,
			locked_until = NULL, reason = NULL, finished_at = NULL
		WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "operations_one_in_flight" {
		return Change{}, ErrInFlight
	}
	if err != nil {
		return Change{}, fmt.Errorf("requeue %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET status = CASE WHEN $2 = 'delete' THEN 'deleting' ELSE status END, reason = NULL
		WHERE id = $1`, l.instanceID, l.kind); err != nil {
		return Change{}, fmt.Errorf("update instance %s: %w", l.instanceID, err)
	}
	s, err := summary(ctx, tx, id)
	return Change{Summary: s, AccountID: l.account}, err
}

// CancelPending ends a pending operation that has never run, so nothing
// on the node was touched. A canceled create leaves its instance deleted,
// which releases its quota; a canceled delete, stop or start withdraws the
// customer's intent, and the instance stays as it is. An operation waiting out a
// backoff has run, and is refused: let it run out, then retry it or leave
// it failed (#102). Run it in a transaction with the audit event.
func CancelPending(ctx context.Context, tx DB, id string) (Change, error) {
	l, err := lock(ctx, tx, id)
	if err != nil {
		return Change{}, err
	}
	switch {
	case l.status != "pending":
		return Change{}, fmt.Errorf("only a pending operation can be canceled; this one is %s", l.status)
	case l.attempts > 0:
		return Change{}, fmt.Errorf("it has run %d time(s) and waits to retry, so the node may hold part of it; "+
			"let it finish or fail, then retry it", l.attempts)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE operations SET status = 'canceled', reason = 'canceled', payload = '{}', finished_at = now()
		WHERE id = $1`, id); err != nil {
		return Change{}, fmt.Errorf("cancel %s: %w", id, err)
	}
	switch l.kind {
	case "create":
		_, err = tx.Exec(ctx, `
			UPDATE instances SET status = 'deleted', desired_state = 'deleted', deleted_at = now(), reason = 'canceled'
			WHERE id = $1`, l.instanceID)
	case "delete", "stop", "start":
		// A stop that failed left it stopping: stopped is what it asked.
		_, err = tx.Exec(ctx, `
			UPDATE instances SET desired_state = CASE WHEN status IN ('stopped', 'stopping') THEN 'stopped' ELSE 'running' END
			WHERE id = $1`, l.instanceID)
	}
	if err != nil {
		return Change{}, fmt.Errorf("update instance %s: %w", l.instanceID, err)
	}
	s, err := summary(ctx, tx, id)
	return Change{Summary: s, AccountID: l.account}, err
}
