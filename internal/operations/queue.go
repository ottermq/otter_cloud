package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/instances"
)

// DB is what the queue needs from a connection, a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const (
	// DefaultLease is how long a claim holds before another worker may take
	// the operation over; a running worker renews it (architecture.md §5).
	DefaultLease = 5 * time.Minute
	// MaxAttempts bounds the runs of one operation, crashes included.
	MaxAttempts = 5
)

var (
	// ErrNotAWorker means the role connected is no node's worker.
	ErrNotAWorker = errors.New("this database role is not a node's worker role (otter_node_<name>, a member of otter_worker)")
	// ErrLeaseLost means the claim ended: the lease expired and another
	// worker took the operation over, or it was finished elsewhere.
	ErrLeaseLost = errors.New("the lease on this operation was lost")
)

// Op is a claimed operation.
type Op struct {
	ID          string
	InstanceID  string
	NodeID      string
	Kind        string
	Step        string // the last completed step; "" before the first
	Attempts    int    // this run included
	Payload     json.RawMessage
	RequestedBy string
	Lock        string // this claim's token, in locked_by
}

// NodeID is the node whose worker the connection's role is.
func NodeID(ctx context.Context, db DB) (string, error) {
	var id *string
	if err := db.QueryRow(ctx, `SELECT worker_node_id()`).Scan(&id); err != nil {
		return "", fmt.Errorf("look up the worker's node: %w", err)
	}
	if id == nil {
		return "", ErrNotAWorker
	}
	return *id, nil
}

// Channel is what the node's worker LISTENs on: an operation that becomes
// pending notifies it. Keep in step with ops_channel() in 0012_worker.sql.
func Channel(nodeID string) string {
	b := []byte("otter_ops_")
	for _, c := range nodeID {
		if c != '-' {
			b = append(b, byte(c))
		}
	}
	return string(b)
}

// Claim takes the next operation of one of kinds that is due on the
// worker's node: a pending one whose backoff is over, or a running one whose
// lease expired, oldest due first. Concurrent claims skip each other's rows,
// so two workers never get the same operation. Row-level security keeps it
// to the role's own node. It returns false when nothing is due.
//
// worker names the process in locked_by; each claim gets a token of its
// own, so a restarted worker can't mistake an old claim for its own.
func Claim(ctx context.Context, db DB, worker string, kinds []string, lease time.Duration) (Op, bool, error) {
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		return Op{}, false, err
	}
	lock := worker + "/" + hex.EncodeToString(token)
	var op Op
	var step *string
	err := db.QueryRow(ctx, `
		UPDATE operations SET status = 'running', attempts = attempts + 1, locked_by = $1,
			locked_until = now() + $2 * interval '1 millisecond', started_at = coalesce(started_at, now())
		WHERE id = (
			SELECT id FROM operations
			WHERE node_id = worker_node_id() AND kind = ANY($3)
			  AND ((status = 'pending' AND next_attempt_at <= now())
			    OR (status = 'running' AND locked_until < now()))
			ORDER BY CASE status WHEN 'running' THEN locked_until ELSE next_attempt_at END, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, instance_id, node_id, kind, step, attempts, payload, requested_by`,
		lock, lease.Milliseconds(), kinds).
		Scan(&op.ID, &op.InstanceID, &op.NodeID, &op.Kind, &step, &op.Attempts, &op.Payload, &op.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Op{}, false, nil
	}
	if err != nil {
		return Op{}, false, fmt.Errorf("claim an operation: %w", err)
	}
	if step != nil {
		op.Step = *step
	}
	op.Lock = lock
	return op, true, nil
}

// Renew extends the claim's lease.
func Renew(ctx context.Context, db DB, op Op, lease time.Duration) error {
	return held(db.Exec(ctx, `
		UPDATE operations SET locked_until = now() + $3 * interval '1 millisecond'
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`, op.ID, op.Lock, lease.Milliseconds()))
}

// Checkpoint records step as done, so a retry resumes after it.
func Checkpoint(ctx context.Context, db DB, op *Op, step string) error {
	err := held(db.Exec(ctx, `
		UPDATE operations SET step = $3 WHERE id = $1 AND locked_by = $2 AND status = 'running'`,
		op.ID, op.Lock, step))
	if err == nil {
		op.Step = step
	}
	return err
}

// Succeed finishes the operation. Its payload is cleared: it may hold a
// verifier, needed no longer. Run it in the transaction that records the
// outcome on the instance.
func Succeed(ctx context.Context, db DB, op Op) error {
	return held(db.Exec(ctx, `
		UPDATE operations SET status = 'succeeded', finished_at = now(), locked_by = NULL, locked_until = NULL,
			payload = '{}', reason = NULL
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`, op.ID, op.Lock))
}

// Fail finishes the operation as failed. reason is the customer-safe code
// (instances.ReasonMessage); cause goes to last_error, for operators only.
// A failed create or delete leaves the instance failed with the same
// reason, in the same statement (domain-model.md §4): what it holds on the
// node is unknown until an operator looks. A failed stop, start or restart
// leaves its status, stopping or starting, with the reason: the customer
// asks again (#95). A deleted instance stays deleted: the failed delete of
// an orphan its reconciler found is the operator's (#96).
func Fail(ctx context.Context, db DB, op Op, reason string, cause error) error {
	if reason != "" && !instances.IsReason(reason) {
		return fmt.Errorf("reason %q is not on the list in instances/reasons.go", reason)
	}
	var n int
	err := db.QueryRow(ctx, `
		WITH op AS (
			UPDATE operations SET status = 'failed', finished_at = now(), locked_by = NULL, locked_until = NULL,
				payload = '{}', reason = nullif($3, ''), last_error = nullif($4, '')
			WHERE id = $1 AND locked_by = $2 AND status = 'running'
			RETURNING instance_id, kind),
		instance AS (
			UPDATE instances i SET status = CASE WHEN op.kind IN ('create', 'delete') THEN 'failed' ELSE i.status END,
				reason = coalesce(nullif($3, ''), $5)
			FROM op WHERE i.id = op.instance_id AND op.kind IN ('create', 'delete', 'stop', 'start', 'restart')
				AND i.status <> 'deleted')
		SELECT count(*) FROM op`, op.ID, op.Lock, reason, errorText(cause), FailReason(op.Kind)).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Retry gives the operation back to the queue after a failed run, due again
// after a backoff that grows with each attempt. After MaxAttempts runs it
// fails it instead, with the kind's reason, and reports true.
func Retry(ctx context.Context, db DB, op Op, cause error) (failed bool, err error) {
	if op.Attempts >= MaxAttempts {
		return true, Fail(ctx, db, op, FailReason(op.Kind), cause)
	}
	return false, held(db.Exec(ctx, `
		UPDATE operations SET status = 'pending', next_attempt_at = now() + $3 * interval '1 millisecond',
			locked_by = NULL, locked_until = NULL, last_error = $4
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`,
		op.ID, op.Lock, Backoff(op.Attempts).Milliseconds(), errorText(cause)))
}

// Backoff is the wait after the given attempt failed: 10 s, doubling, at
// most 10 minutes.
func Backoff(attempt int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < attempt && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

// FailReason is the customer-safe code for an operation of kind that ran
// out of attempts; "" for a kind without one.
func FailReason(kind string) string {
	if r := kind + "_failed"; instances.IsReason(r) {
		return r
	}
	return ""
}

func held(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// errorText keeps last_error to a size an operator reads.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 2000 {
		s = strings.ToValidUTF8(s[:2000], "") + "…"
	}
	return s
}
