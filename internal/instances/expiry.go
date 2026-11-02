package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/audit"
)

// ExpiryGrace is how long an expired instance stays stopped before it is
// deleted (Q7).
const ExpiryGrace = 7 * 24 * time.Hour

// DefaultExpireEvery is how often the api applies the expiry policy.
const DefaultExpireEvery = time.Minute

// TxDB is a DB that begins transactions.
type TxDB interface {
	DB
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Expired is one step Expire took: a stop or a delete queued for an
// instance.
type Expired struct {
	InstanceID string
	Kind       string // stop, delete
}

// Expire applies Q7 to the instances whose expires_at is past, as of now
// (#100). It records intent, as a customer's request does; the node's
// worker runs the operation:
//
//   - a running one is stopped: its reason shows as expired while it is
//     stopped past its expiry (instanceViewQuery);
//   - ExpiryGrace after expires_at it is deleted, and the delete takes the
//     final dump first, kept the expiry's retention. An ephemeral one is
//     deleted at expires_at, without a dump.
//
// An instance with an operation in flight is left to it and looked at
// again next time; one being created, failed or deleted is not touched.
// Each step commits with its audit event, as the system's.
func Expire(ctx context.Context, db TxDB, now time.Time) ([]Expired, error) {
	rows, err := db.Query(ctx, `
		SELECT id FROM instances
		WHERE expires_at <= $1 AND status IN ('running', 'starting', 'stopping', 'stopped')
		  AND desired_state <> 'deleted'
		ORDER BY expires_at, id`, now)
	if err != nil {
		return nil, fmt.Errorf("list expired instances: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list expired instances: %w", err)
	}
	var done []Expired
	for _, id := range ids {
		kind, err := expireOne(ctx, db, id, now)
		if err != nil {
			return done, err
		}
		if kind != "" {
			done = append(done, Expired{InstanceID: id, Kind: kind})
		}
	}
	return done, nil
}

func expireOne(ctx context.Context, db TxDB, id string, now time.Time) (string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		accountID, nodeID, status, desired string
		ephemeral, busy                    bool
		expiresAt                          time.Time
	)
	// Locked and judged again: the customer may have moved the expiry, or
	// asked for something, since the list.
	err = tx.QueryRow(ctx, `
		SELECT i.account_id, i.node_id, i.status, i.desired_state, i.ephemeral, i.expires_at,
			EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.status IN ('pending', 'running'))
		FROM instances i WHERE i.id = $1 AND i.expires_at <= $2 FOR UPDATE OF i`, id, now).
		Scan(&accountID, &nodeID, &status, &desired, &ephemeral, &expiresAt, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lock instance %s: %w", id, err)
	}
	var kind string
	switch {
	case busy || desired == "deleted":
		return "", nil
	case ephemeral || !now.Before(expiresAt.Add(ExpiryGrace)):
		kind = "delete"
	case desired == "stopped" && status == "stopped":
		return "", nil
	default:
		kind = "stop"
	}
	desiredNow := map[string]string{"stop": "stopped", "delete": "deleted"}[kind]
	if _, err := tx.Exec(ctx, `UPDATE instances SET desired_state = $2 WHERE id = $1`, id, desiredNow); err != nil {
		return "", fmt.Errorf("record instance %s expiring: %w", id, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		VALUES ($1, $2, $3, $4, 'system')`, id, accountID, nodeID, kind)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "operations_one_in_flight" {
		return "", nil // queued since the lock was taken: next time
	}
	if err != nil {
		return "", fmt.Errorf("queue the expiry %s of instance %s: %w", kind, id, err)
	}
	details := map[string]any{"step": kind, "expires_at": expiresAt.UTC()}
	if ephemeral {
		details["ephemeral"] = true
	}
	if err := audit.Record(ctx, tx, audit.Event{Actor: audit.System, AccountID: accountID, Action: audit.ActionInstanceExpire,
		TargetType: "instance", TargetID: id, Outcome: audit.OK, Details: details}); err != nil {
		return "", err
	}
	return kind, tx.Commit(ctx)
}

// RunExpiry applies the expiry policy now and every every
// (DefaultExpireEvery when zero) until ctx ends, logging what it does.
func RunExpiry(ctx context.Context, db TxDB, log *slog.Logger, every time.Duration) {
	if every <= 0 {
		every = DefaultExpireEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		done, err := Expire(ctx, db, time.Now())
		for _, e := range done {
			log.Info("expiry: "+e.Kind+" queued", "instance", e.InstanceID)
		}
		if err != nil && ctx.Err() == nil {
			log.Error("expiry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
