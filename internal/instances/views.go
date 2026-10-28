package instances

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/billing"
)

// InstanceView is an instance as its owner sees it (portals.md §1): status
// and health apart, a reason as a code and a message, the allocation, and
// connection details once it runs. No cluster, node or tenant address.
type InstanceView struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	ServiceType  string          `json:"service_type"`
	Flavor       string          `json:"flavor"`
	Status       string          `json:"status"`
	Health       string          `json:"health"`
	DesiredState string          `json:"desired_state"`
	Reason       string          `json:"reason,omitempty"`
	Message      string          `json:"message,omitempty"`
	Allocation   AllocationView  `json:"allocation"`
	Connection   *ConnectionView `json:"connection"` // null until it runs
	Operation    *OperationView  `json:"operation"`  // the one in flight, or null
	ExpiresAt    *time.Time      `json:"expires_at"`
	CreatedAt    time.Time       `json:"created_at"`
}

// AllocationView is what the instance holds of the account's quota.
type AllocationView struct {
	StorageGB      int  `json:"storage_gb"`
	MaxConnections *int `json:"max_connections,omitempty"`
}

// ConnectionView is how to reach a running database. The password is not
// here: it was shown once, at creation (#54).
type ConnectionView struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	TLS      string `json:"tls"` // always "required"
}

// instanceViewQuery selects the view's columns, with the instance's
// in-flight operation if it has one, for an instances table called i.
const instanceViewQuery = `
	SELECT i.id, i.name, i.service_type, i.flavor_code, i.status, i.health, i.desired_state, coalesce(i.reason, ''),
		i.storage_gb, i.max_connections, i.infra_name, coalesce(i.endpoint_host, ''), coalesce(i.endpoint_port, 0),
		i.expires_at, i.created_at,
		o.id, o.kind, o.status, coalesce(o.reason, ''), o.requested_by, o.created_at, o.finished_at
	FROM instances i
	LEFT JOIN operations o ON o.instance_id = i.id AND o.status IN ('pending', 'running')`

func scanInstanceView(row pgx.CollectableRow) (InstanceView, error) {
	var (
		v              InstanceView
		infra, host    string
		port           int
		opID, kind, st *string
		reason, by     *string
		opAt           *time.Time
		opFinished     *time.Time
	)
	err := row.Scan(&v.ID, &v.Name, &v.ServiceType, &v.Flavor, &v.Status, &v.Health, &v.DesiredState, &v.Reason,
		&v.Allocation.StorageGB, &v.Allocation.MaxConnections, &infra, &host, &port, &v.ExpiresAt, &v.CreatedAt,
		&opID, &kind, &st, &reason, &by, &opAt, &opFinished)
	if err != nil {
		return InstanceView{}, err
	}
	v.Message = ReasonMessage(v.Reason)
	v.CreatedAt = v.CreatedAt.UTC()
	if v.ExpiresAt != nil {
		t := v.ExpiresAt.UTC()
		v.ExpiresAt = &t
	}
	if v.Status == "running" && host != "" {
		v.Connection = &ConnectionView{Host: host, Port: port, Database: infra, User: infra, TLS: "required"}
	}
	if opID != nil {
		v.Operation = &OperationView{ID: *opID, InstanceID: v.ID, Kind: *kind, Status: *st, Reason: *reason,
			Message: ReasonMessage(*reason), RequestedBy: requester(*by), CreatedAt: opAt.UTC(), FinishedAt: opFinished}
	}
	return v, nil
}

// ListInstances lists the account's instances that aren't deleted, oldest
// first.
func ListInstances(ctx context.Context, db DB, accountID string) ([]InstanceView, error) {
	rows, err := db.Query(ctx, instanceViewQuery+`
		WHERE i.account_id = $1 AND i.status <> 'deleted' ORDER BY i.created_at, i.id`, accountID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanInstanceView)
}

// InstanceByID returns the account's instance with id, deleted or not.
func InstanceByID(ctx context.Context, db DB, accountID, id string) (InstanceView, error) {
	if !isUUID(id) {
		return InstanceView{}, ErrNotFound
	}
	rows, err := db.Query(ctx, instanceViewQuery+` WHERE i.id = $1 AND i.account_id = $2`, id, accountID)
	if err != nil {
		return InstanceView{}, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, scanInstanceView)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceView{}, ErrNotFound
	}
	return v, err
}

// Errors from Delete.
var (
	ErrConfirmMismatch = errors.New("type the database's name to confirm deleting it")
	ErrBusy            = errors.New("something is already happening to this database; wait for it to finish")
	ErrAlreadyDeleted  = errors.New("this database is already deleted or being deleted")
)

// Delete records the intent to delete the account's instance id and queues
// its delete operation. confirm must be the instance's name (security.md
// R12). One operation runs at a time per instance, so a database still
// being created can't be deleted until the create ends. Run it in a
// transaction with the audit event.
func Delete(ctx context.Context, tx pgx.Tx, accountID, id, confirm, requestedBy string) (InstanceView, OperationView, error) {
	if !isUUID(id) {
		return InstanceView{}, OperationView{}, ErrNotFound
	}
	var name, desired, status, nodeID string
	err := tx.QueryRow(ctx, `SELECT name, desired_state, status, node_id FROM instances
		WHERE id = $1 AND account_id = $2 FOR UPDATE`, id, accountID).Scan(&name, &desired, &status, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceView{}, OperationView{}, ErrNotFound
	}
	if err != nil {
		return InstanceView{}, OperationView{}, fmt.Errorf("lock instance: %w", err)
	}
	if confirm != name {
		return InstanceView{}, OperationView{}, ErrConfirmMismatch
	}
	if desired == "deleted" || status == "deleting" || status == "deleted" {
		return InstanceView{}, OperationView{}, ErrAlreadyDeleted
	}
	var opID string
	err = tx.QueryRow(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		VALUES ($1, $2, $3, 'delete', $4) RETURNING id`, id, accountID, nodeID, requestedBy).Scan(&opID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "operations_one_in_flight" {
		return InstanceView{}, OperationView{}, ErrBusy
	}
	if err != nil {
		return InstanceView{}, OperationView{}, fmt.Errorf("queue delete: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET desired_state = 'deleted' WHERE id = $1`, id); err != nil {
		return InstanceView{}, OperationView{}, fmt.Errorf("record intent: %w", err)
	}
	v, err := InstanceByID(ctx, tx, accountID, id)
	if err != nil {
		return InstanceView{}, OperationView{}, err
	}
	op, err := OperationByID(ctx, tx, accountID, opID)
	return v, op, err
}

// Usage is what an account's instances hold: every instance that isn't
// deleted, failed ones included. The quota check and the subscription page
// both use it, so they can't disagree.
type Usage struct {
	Instances   int
	StorageGB   int
	Connections int
	MemoryMB    int
	VCPU        int
}

// AccountUsage sums the account's allocations.
func AccountUsage(ctx context.Context, db DB, accountID string) (Usage, error) {
	var u Usage
	err := db.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(storage_gb), 0), coalesce(sum(max_connections), 0),
			coalesce(sum(memory_mb), 0), coalesce(sum(vcpu), 0)
		FROM instances WHERE account_id = $1 AND status <> 'deleted'`, accountID).
		Scan(&u.Instances, &u.StorageGB, &u.Connections, &u.MemoryMB, &u.VCPU)
	if err != nil {
		return Usage{}, fmt.Errorf("sum the account's instances: %w", err)
	}
	return u, nil
}

// LimitView is one limit of the account's subscription with what is
// allocated against it.
type LimitView struct {
	Allowed   int `json:"allowed"`
	Allocated int `json:"allocated"`
}

// Allowance pairs each nonzero limit with the account's usage, keyed as
// QuotaError names limits. Memory and vCPU appear only when the plan
// allows them.
func Allowance(l billing.Limits, u Usage) map[string]LimitView {
	m := map[string]LimitView{
		"instances":   {l.Instances, u.Instances},
		"storage_gb":  {l.StorageGB, u.StorageGB},
		"connections": {l.Connections, u.Connections},
	}
	if l.MemoryMB > 0 {
		m["memory_mb"] = LimitView{l.MemoryMB, u.MemoryMB}
	}
	if l.VCPU > 0 {
		m["vcpu"] = LimitView{l.VCPU, u.VCPU}
	}
	return m
}
