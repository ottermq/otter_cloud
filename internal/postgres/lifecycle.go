package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// The steps of a stop, start or restart, in operations.step as each is
// done, beside StepUnrouted, StepClosed and StepPool.
const (
	StepOpened = "opened"
	StepEnded  = "ended"
)

// Stop stops the shared database of a stop operation (architecture.md §6,
// Lifecycle; #95): the instance is stopping; its PgBouncer entry goes,
// which ends its clients there and refuses new ones; PostgreSQL refuses new
// connections to it (ALLOW_CONNECTIONS false) and ends the open ones; then
// the instance is stopped. Its role, data and quota stay.
func (d *Driver) Stop(ctx context.Context, job *operations.Job) error {
	t, conn, err := d.lifecycleTarget(ctx, job, "stopped", "stopping")
	if err != nil || conn == nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	// desired_state is stopped, so the entry is not listed.
	if err := d.ensurePool(ctx, job.DB(), t); err != nil {
		return err
	}
	if err := job.Checkpoint(ctx, StepUnrouted); err != nil {
		return err
	}
	if err := allowConnections(ctx, conn, t.tenant.Name, false); err != nil {
		return err
	}
	if err := endSessions(ctx, conn, t.tenant.Name); err != nil {
		return err
	}
	if err := job.Checkpoint(ctx, StepClosed); err != nil {
		return err
	}
	return recordStatus(ctx, job.DB(), t.instanceID, "stopped")
}

// Start reverses Stop: PostgreSQL takes connections to the database again,
// then PgBouncer routes to it, then the instance is running.
func (d *Driver) Start(ctx context.Context, job *operations.Job) error {
	t, conn, err := d.lifecycleTarget(ctx, job, "running", "starting")
	if err != nil || conn == nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if err := d.open(ctx, job, conn, t); err != nil {
		return err
	}
	return recordStatus(ctx, job.DB(), t.instanceID, "running")
}

// Restart ends a running database's connections and lets clients connect
// again: a shared database has no server of its own to restart. PgBouncer
// drops its clients and server connections (KILL) and takes new ones
// (RESUME); PostgreSQL ends whatever is left. On the way it makes sure the
// database is open and routed, as Start does.
func (d *Driver) Restart(ctx context.Context, job *operations.Job) error {
	t, conn, err := d.lifecycleTarget(ctx, job, "running", "starting")
	if err != nil || conn == nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if err := d.open(ctx, job, conn, t); err != nil {
		return err
	}
	pool := d.Pool
	pool.Jail = t.jail
	if err := Reconnect(ctx, d.JailDeck, pool, t.tenant.Name); err != nil {
		return err
	}
	if err := endSessions(ctx, conn, t.tenant.Name); err != nil {
		return err
	}
	if err := job.Checkpoint(ctx, StepEnded); err != nil {
		return err
	}
	return recordStatus(ctx, job.DB(), t.instanceID, "running")
}

// lifecycleTarget loads the operation's instance, checks that it still
// wants what the operation does, records the transitional status, and
// connects as the cluster's admin. A nil conn and error mean there is
// nothing to do: the instance is already as desired, recorded by a run
// that crashed before finishing the operation.
func (d *Driver) lifecycleTarget(ctx context.Context, job *operations.Job, desired, transitional string) (target, *pgx.Conn, error) {
	reason := operations.FailReason(job.Kind)
	t, err := d.load(ctx, job.DB(), job.InstanceID, reason)
	if err != nil {
		return t, nil, err
	}
	switch {
	case t.desired != desired:
		// Only a retry of a superseded operation gets here; RetryFailed
		// refuses one.
		return t, nil, operations.Permanent(reason, fmt.Errorf("instance %s should be %s, not %s", t.instanceID, t.desired, desired))
	case !slices.Contains([]string{"running", "stopping", "stopped", "starting"}, t.status):
		return t, nil, operations.Permanent(reason, fmt.Errorf("instance %s is %s", t.instanceID, t.status))
	case t.status == desired && job.Kind != "restart":
		return t, nil, nil
	}
	if !namePattern.MatchString(t.tenant.Name) {
		return t, nil, operations.Permanent(reason, fmt.Errorf("tenant name %q is not otc_<12 hex>", t.tenant.Name))
	}
	if err := recordStatus(ctx, job.DB(), t.instanceID, transitional); err != nil {
		return t, nil, err
	}
	conn, err := d.connect(ctx, &t)
	if err != nil {
		return t, nil, err
	}
	// The database must be the tenant's, as the worker made it: a missing
	// one is the reconciler's to report (#96), never recreated here.
	var owner string
	err = conn.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, t.tenant.Name).Scan(&owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = operations.Permanent(reason, fmt.Errorf("database %s does not exist", t.tenant.Name))
	case err != nil:
		err = fmt.Errorf("look up database %s: %w", t.tenant.Name, err)
	case owner != t.tenant.Name:
		err = operations.Permanent(reason, fmt.Errorf("%w: database %s is owned by %s", ErrNotOurs, t.tenant.Name, owner))
	}
	if err != nil {
		conn.Close(context.WithoutCancel(ctx))
		return t, nil, err
	}
	return t, conn, nil
}

// open lets PostgreSQL take connections to the database, then routes
// PgBouncer to it.
func (d *Driver) open(ctx context.Context, job *operations.Job, conn *pgx.Conn, t target) error {
	if err := allowConnections(ctx, conn, t.tenant.Name, true); err != nil {
		return err
	}
	if err := job.Checkpoint(ctx, StepOpened); err != nil {
		return err
	}
	if err := d.ensurePool(ctx, job.DB(), t); err != nil {
		return err
	}
	return job.Checkpoint(ctx, StepPool)
}

func recordStatus(ctx context.Context, db operations.DB, instanceID, status string) error {
	// reason goes with the status it explained: a failed stop's, say.
	_, err := db.Exec(ctx, `UPDATE instances SET status = $2, reason = NULL
		WHERE id = $1 AND status NOT IN ('deleting', 'deleted')`, instanceID, status)
	if err != nil {
		return fmt.Errorf("record instance %s %s: %w", instanceID, status, err)
	}
	return nil
}

// allowConnections sets whether PostgreSQL takes new connections to the
// database, from anyone: the tenant, and the worker's admin too.
func allowConnections(ctx context.Context, conn *pgx.Conn, name string, allow bool) error {
	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS %t", pgx.Identifier{name}.Sanitize(), allow)); err != nil {
		return fmt.Errorf("database %s: allow connections %t: %w", name, allow, err)
	}
	return nil
}

// endSessions ends every session on the database. conn is on another one.
func endSessions(ctx context.Context, conn *pgx.Conn, name string) error {
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid()`, name); err != nil {
		return fmt.Errorf("end sessions on %s: %w", name, err)
	}
	return nil
}

// Reconnect drops every client and server connection PgBouncer has for the
// database, and takes new ones again: KILL, then RESUME. Both are safe to
// repeat, so a rerun after a crash between them finishes the job.
func Reconnect(ctx context.Context, jd *jaildeck.Client, p Pool, name string) error {
	// The name goes into a console command: only the platform's shape.
	if !namePattern.MatchString(name) {
		return fmt.Errorf("tenant name %q is not otc_<12 hex>", name)
	}
	p = p.defaults()
	if _, err := p.console(ctx, jd, "KILL "+name); err != nil {
		return fmt.Errorf("PgBouncer KILL %s: %w", name, err)
	}
	if _, err := p.console(ctx, jd, "RESUME "+name); err != nil {
		return fmt.Errorf("PgBouncer RESUME %s: %w", name, err)
	}
	return nil
}
