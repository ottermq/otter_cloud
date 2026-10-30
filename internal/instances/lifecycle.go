package instances

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// StateError means the instance can't take the request in its status. Its
// text is safe to show; the API answers 409.
type StateError struct{ msg string }

func (e *StateError) Error() string { return e.msg }

// lifecycle is what a stop, start or restart may start from, and the
// desired state it records (#95). A stop or start that failed leaves the
// instance stopping or starting, with the operation's reason: the customer
// asks again, either way.
var lifecycle = map[string]struct {
	from    []string
	desired string // "" keeps it
	verb    string
	already map[string]string // status → why there is nothing to do
}{
	"stop": {from: []string{"running", "starting", "stopping"}, desired: "stopped", verb: "stopped",
		already: map[string]string{"stopped": "this database is already stopped"}},
	"start": {from: []string{"stopped", "stopping", "starting"}, desired: "running", verb: "started",
		already: map[string]string{"running": "this database is already running"}},
	"restart": {from: []string{"running", "starting"}, verb: "restarted",
		already: map[string]string{"stopped": "this database is stopped; start it instead",
			"stopping": "this database is stopped; start it instead"}},
}

// Stop queues a stop of the account's instance id: new connections are
// refused and open ones ended, and it keeps everything it holds, quota
// included. Run it in a transaction with the audit event.
func Stop(ctx context.Context, tx pgx.Tx, accountID, id, requestedBy string) (InstanceView, OperationView, error) {
	return queueLifecycle(ctx, tx, "stop", accountID, id, requestedBy)
}

// Start queues a start of the account's stopped instance id.
func Start(ctx context.Context, tx pgx.Tx, accountID, id, requestedBy string) (InstanceView, OperationView, error) {
	return queueLifecycle(ctx, tx, "start", accountID, id, requestedBy)
}

// Restart queues a restart of the account's running instance id. A shared
// database has no server of its own: a restart ends its connections, at
// PgBouncer and in PostgreSQL, and clients connect again.
func Restart(ctx context.Context, tx pgx.Tx, accountID, id, requestedBy string) (InstanceView, OperationView, error) {
	return queueLifecycle(ctx, tx, "restart", accountID, id, requestedBy)
}

func queueLifecycle(ctx context.Context, tx pgx.Tx, kind, accountID, id, requestedBy string) (InstanceView, OperationView, error) {
	l := lifecycle[kind]
	if !isUUID(id) {
		return InstanceView{}, OperationView{}, ErrNotFound
	}
	var desired, status, nodeID string
	err := tx.QueryRow(ctx, `SELECT desired_state, status, node_id FROM instances
		WHERE id = $1 AND account_id = $2 FOR UPDATE`, id, accountID).Scan(&desired, &status, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceView{}, OperationView{}, ErrNotFound
	}
	if err != nil {
		return InstanceView{}, OperationView{}, fmt.Errorf("lock instance: %w", err)
	}
	switch {
	case desired == "deleted" || status == "deleting" || status == "deleted":
		return InstanceView{}, OperationView{}, ErrAlreadyDeleted
	case l.already[status] != "":
		return InstanceView{}, OperationView{}, &StateError{l.already[status]}
	case !slices.Contains(l.from, status):
		return InstanceView{}, OperationView{}, &StateError{fmt.Sprintf("a database that is %s can't be %s", status, l.verb)}
	}
	var opID string
	err = tx.QueryRow(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, id, accountID, nodeID, kind, requestedBy).Scan(&opID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "operations_one_in_flight" {
		return InstanceView{}, OperationView{}, ErrBusy
	}
	if err != nil {
		return InstanceView{}, OperationView{}, fmt.Errorf("queue %s: %w", kind, err)
	}
	if l.desired != "" {
		if _, err := tx.Exec(ctx, `UPDATE instances SET desired_state = $2 WHERE id = $1`, id, l.desired); err != nil {
			return InstanceView{}, OperationView{}, fmt.Errorf("record intent: %w", err)
		}
	}
	v, err := InstanceByID(ctx, tx, accountID, id)
	if err != nil {
		return InstanceView{}, OperationView{}, err
	}
	op, err := OperationByID(ctx, tx, accountID, opID)
	return v, op, err
}
