package instances

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/scram"
)

// Reset is what ResetPassword queued, or found. Password is the new one,
// generated for this reset and kept nowhere: the caller shows it in its
// response and nowhere else. Replayed means the request's Idempotency-Key
// named an earlier reset, and nothing new was queued; Password is then
// empty, since only the first response had it.
type Reset struct {
	Instance  InstanceView
	Operation OperationView
	Password  string
	Replayed  bool
}

// resettable are the statuses whose role is there to take a new password:
// a stopped database's too, so it is ready when started.
var resettable = []string{"running", "stopped", "stopping", "starting"}

// ResetPassword queues a reset_password of the account's instance id
// (#101): a new password, of which only the SCRAM verifier is stored, in
// the operation's payload, cleared when it finishes (#54). The old password
// works until the worker sets the new one; connections already open stay
// open (a restart ends them). Run it in a transaction with the audit event.
func ResetPassword(ctx context.Context, tx pgx.Tx, accountID, id, requestedBy, idempotencyKey string) (Reset, error) {
	if !isUUID(id) {
		return Reset{}, ErrNotFound
	}
	var desired, status, nodeID string
	err := tx.QueryRow(ctx, `SELECT desired_state, status, node_id FROM instances
		WHERE id = $1 AND account_id = $2 FOR UPDATE`, id, accountID).Scan(&desired, &status, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reset{}, ErrNotFound
	}
	if err != nil {
		return Reset{}, fmt.Errorf("lock instance: %w", err)
	}

	// A replay is answered before the status is checked: the request
	// already succeeded.
	if idempotencyKey != "" {
		var kind, instanceID, opID string
		err := tx.QueryRow(ctx, `SELECT kind, instance_id, id FROM operations WHERE account_id = $1 AND idempotency_key = $2`,
			accountID, idempotencyKey).Scan(&kind, &instanceID, &opID)
		switch {
		case err == nil && (kind != "reset_password" || instanceID != id):
			return Reset{}, ErrIdempotencyMismatch
		case err == nil:
			r, err := resetViews(ctx, tx, accountID, id, opID)
			r.Replayed = true
			return r, err
		case !errors.Is(err, pgx.ErrNoRows):
			return Reset{}, fmt.Errorf("look up Idempotency-Key: %w", err)
		}
	}

	switch {
	case desired == "deleted" || status == "deleting" || status == "deleted":
		return Reset{}, ErrAlreadyDeleted
	case !slices.Contains(resettable, status):
		return Reset{}, &StateError{fmt.Sprintf("a database that is %s can't have its password reset", status)}
	}
	password := scram.Password()
	verifier, err := scram.Verifier(password)
	if err != nil {
		return Reset{}, err
	}
	var opID string
	err = tx.QueryRow(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by, idempotency_key, payload)
		VALUES ($1, $2, $3, 'reset_password', $4, NULLIF($5, ''), jsonb_build_object('verifier', $6::text))
		RETURNING id`, id, accountID, nodeID, requestedBy, idempotencyKey, verifier).Scan(&opID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "operations_one_in_flight" {
		return Reset{}, ErrBusy
	}
	if err != nil {
		return Reset{}, fmt.Errorf("queue reset_password: %w", err)
	}
	r, err := resetViews(ctx, tx, accountID, id, opID)
	r.Password = password
	return r, err
}

func resetViews(ctx context.Context, tx pgx.Tx, accountID, id, opID string) (Reset, error) {
	v, err := InstanceByID(ctx, tx, accountID, id)
	if err != nil {
		return Reset{}, err
	}
	op, err := OperationByID(ctx, tx, accountID, opID)
	return Reset{Instance: v, Operation: op}, err
}
