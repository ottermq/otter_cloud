package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// Driver runs the shared-database operations of one node's worker.
type Driver struct {
	JailDeck    *jaildeck.Client
	Credentials Credentials
	// Port is PostgreSQL's on each cluster's address; postgres.Port when
	// zero. Only a test cluster has another.
	Port int
	// Pool is each cluster's PgBouncer but for its jail, which comes from
	// the cluster's row; the zero value is a cluster jail's paths.
	Pool Pool
}

// Handlers are the operation kinds the driver runs, for operations.Worker.
func (d *Driver) Handlers() map[string]operations.Handler {
	return map[string]operations.Handler{"create": d.Create}
}

// The steps of a create, in operations.step as each is done.
const (
	StepRole     = "role"
	StepDatabase = "database"
	StepPool     = "pool"
)

// target is an instance and the cluster it is placed in, as the worker
// reads them.
type target struct {
	instanceID, status string
	tenant             Tenant
	clusterID, jail    string
	cluster            Cluster
	pgMajor            int
}

// Create provisions the instance of a create operation: its role and
// database in the cluster, then the cluster's PgBouncer entries, then the
// instance running (architecture.md §4). Every step is an ensure, and a
// rerun after a crash or a retry runs them all again: the checkpoints say
// how far a run got, for the operator, and skip nothing.
//
// What retrying can't fix fails now: a missing verifier, a role or database
// of the instance's name that the worker did not make, a render that fails
// its own check.
func (d *Driver) Create(ctx context.Context, job *operations.Job) error {
	t, err := d.load(ctx, job.DB(), job.InstanceID)
	if err != nil {
		return err
	}
	switch t.status {
	case "running":
		return nil // recorded by a run that crashed before finishing the operation
	case "provisioning":
	default:
		return operations.Permanent("create_failed", fmt.Errorf("instance %s is %s, not provisioning", t.instanceID, t.status))
	}
	var payload struct {
		Verifier string `json:"verifier"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.Verifier == "" {
		return operations.Permanent("create_failed", errors.New("the operation has no password verifier"))
	}
	t.tenant.Verifier = payload.Verifier
	if err := t.tenant.check(); err != nil {
		return operations.Permanent("create_failed", err)
	}

	t.cluster.ServerCert, err = ServerCert(ctx, d.JailDeck, t.jail, t.pgMajor)
	if err != nil {
		return err
	}
	conn, err := Connect(ctx, t.cluster, d.Credentials)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))

	if err := EnsureRole(ctx, conn, t.tenant); err != nil {
		return permanentIf(err, ErrUnsafeRole)
	}
	if err := job.Checkpoint(ctx, StepRole); err != nil {
		return err
	}
	connect := func(ctx context.Context, database string) (*pgx.Conn, error) {
		return ConnectDB(ctx, t.cluster, d.Credentials, database)
	}
	if err := EnsureDatabase(ctx, conn, connect, t.tenant); err != nil {
		return permanentIf(err, ErrUnsafeRole)
	}
	if err := job.Checkpoint(ctx, StepDatabase); err != nil {
		return err
	}
	entries, err := PoolEntries(ctx, job.DB(), t.clusterID)
	if err != nil {
		return err
	}
	pool := d.Pool
	pool.Jail = t.jail
	if _, err := EnsurePoolEntries(ctx, d.JailDeck, pool, entries); err != nil {
		return permanentIf(err, ErrPoolRender)
	}
	if err := job.Checkpoint(ctx, StepPool); err != nil {
		return err
	}
	_, err = job.DB().Exec(ctx, `UPDATE instances SET status = 'running', reason = NULL WHERE id = $1 AND status = 'provisioning'`, t.instanceID)
	if err != nil {
		return fmt.Errorf("record instance %s running: %w", t.instanceID, err)
	}
	return nil
}

func (d *Driver) load(ctx context.Context, db operations.DB, instanceID string) (target, error) {
	t := target{instanceID: instanceID, cluster: Cluster{Port: d.Port}}
	err := db.QueryRow(ctx, `
		SELECT i.status, i.infra_name, i.storage_gb, c.id, c.jail_name, host(c.pool_addr), c.pg_major
		FROM instances i JOIN clusters c ON c.id = i.cluster_id
		WHERE i.id = $1`, instanceID).
		Scan(&t.status, &t.tenant.Name, &t.tenant.StorageGB, &t.clusterID, &t.jail, &t.cluster.Host, &t.pgMajor)
	if errors.Is(err, pgx.ErrNoRows) {
		// Row-level security: the instance is not on this worker's node.
		return t, operations.Permanent("create_failed", fmt.Errorf("instance %s is not on this node", instanceID))
	}
	if err != nil {
		return t, fmt.Errorf("read instance %s: %w", instanceID, err)
	}
	return t, nil
}

// permanentIf makes err permanent when it is target: retrying won't help.
func permanentIf(err, target error) error {
	if errors.Is(err, target) {
		return operations.Permanent("create_failed", err)
	}
	return err
}
