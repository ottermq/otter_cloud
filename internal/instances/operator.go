package instances

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OperatorInstance is an instance as an operator lists it (`otter admin
// instances list`, #102): whose it is and where it lives, which a customer
// never sees.
type OperatorInstance struct {
	ID           string
	Owner        string // the account's first user's email
	Name         string
	Flavor       string
	Status       string
	DesiredState string
	Reason       string
	Node         string
	Cluster      string // "" for an instance outside a cluster
	InfraName    string
	CreatedAt    time.Time
}

// OperatorInstances returns instances, newest first; deleted ones only with
// withDeleted.
func OperatorInstances(ctx context.Context, db DB, withDeleted bool) ([]OperatorInstance, error) {
	rows, err := db.Query(ctx, `
		SELECT i.id, coalesce((SELECT min(u.email) FROM users u WHERE u.account_id = i.account_id), ''), i.name,
			i.flavor_code, i.status, i.desired_state, coalesce(i.reason, ''), n.name, coalesce(c.name, ''),
			i.infra_name, i.created_at
		FROM instances i JOIN infrastructure_nodes n ON n.id = i.node_id LEFT JOIN clusters c ON c.id = i.cluster_id
		WHERE $1 OR i.status <> 'deleted'
		ORDER BY i.created_at DESC, i.id DESC`, withDeleted)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OperatorInstance, error) {
		var v OperatorInstance
		err := row.Scan(&v.ID, &v.Owner, &v.Name, &v.Flavor, &v.Status, &v.DesiredState, &v.Reason, &v.Node,
			&v.Cluster, &v.InfraName, &v.CreatedAt)
		return v, err
	})
}

// GiB is what one allocated GB of storage is, in bytes (Q27): the
// allocation is generous rather than short.
const GiB = 1 << 30

// CleanupWindow is how long a reopened database stays read-only before it
// is blocked again, if still over the blocking point (Q27).
const CleanupWindow = 24 * time.Hour

// ErrNotBlocked means the instance isn't blocked over its storage
// allocation: there is nothing to reopen.
var ErrNotBlocked = errors.New("the database is not blocked over its storage allocation")

// Reopened is what Reopen did.
type Reopened struct {
	ID, AccountID, Name, InfraName string
	CleanupUntil                   time.Time
}

// Reopen lets a database blocked over its storage allocation take
// connections again, read-only, for CleanupWindow (Q27, #99): its owner
// deletes data to get back under. The node's worker opens it at its next
// storage check; still over the blocking point when the window ends, it is
// blocked again. Run it in a transaction with the audit event.
func Reopen(ctx context.Context, db DB, id string) (Reopened, error) {
	if !isUUID(id) {
		return Reopened{}, ErrNotFound
	}
	r := Reopened{ID: id}
	var state string
	err := db.QueryRow(ctx, `SELECT account_id, name, infra_name, storage_state FROM instances
		WHERE id = $1 AND status <> 'deleted' FOR UPDATE`, id).Scan(&r.AccountID, &r.Name, &r.InfraName, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reopened{}, ErrNotFound
	}
	if err != nil {
		return Reopened{}, fmt.Errorf("lock instance: %w", err)
	}
	if state != "blocked" {
		return Reopened{}, ErrNotBlocked
	}
	if err := db.QueryRow(ctx, `UPDATE instances SET storage_state = 'read_only', cleanup_until = now() + make_interval(secs => $2)
		WHERE id = $1 RETURNING cleanup_until`, id, CleanupWindow.Seconds()).Scan(&r.CleanupUntil); err != nil {
		return Reopened{}, fmt.Errorf("reopen: %w", err)
	}
	return r, nil
}
