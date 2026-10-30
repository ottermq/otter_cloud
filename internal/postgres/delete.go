package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/operations"
)

// The steps of a delete, in operations.step as each is done.
const (
	StepUnrouted = "unrouted"
	StepClosed   = "closed"
	StepDumped   = "dumped"
	StepDropped  = "dropped"
)

// UserDeleteRetention is how long the final dump of a database its owner
// deleted is kept (Q26). Pruning it is #100's.
const UserDeleteRetention = 3 * 24 * time.Hour

// ErrNotOurs means a role or database of the instance's name is not one the
// worker made: it is left in place for an operator.
var ErrNotOurs = errors.New("the role or database of this name is not one the worker made; left for an operator")

// Delete deletes the shared database of a delete operation
// (architecture.md §6, Lifecycle):
//
//  1. the instance is deleting, and the cluster's PgBouncer stops routing
//     to it;
//  2. its role may no longer log in, and its sessions are ended;
//  3. the final dump is taken, on the node, outside the cluster jail, and
//     recorded with its expiry;
//  4. only then are the database and the role dropped;
//  5. the instance is deleted, which releases its quota and capacity.
//
// A dump that fails fails this run, with nothing dropped: the operation is
// retried, and after its last attempt it fails with the database in place.
// A rerun reuses its own dump if the file is still as it was written. What
// the worker did not make it never drops: a role with privileges a tenant
// must not have (a create refused it), or a database owned by another role.
func (d *Driver) Delete(ctx context.Context, job *operations.Job) error {
	db := job.DB()
	t, err := d.load(ctx, db, job.InstanceID, "delete_failed")
	if err != nil {
		return err
	}
	if t.status == "deleted" {
		return nil
	}
	if _, err := db.Exec(ctx, `UPDATE instances SET status = 'deleting' WHERE id = $1 AND status <> 'deleted'`, t.instanceID); err != nil {
		return fmt.Errorf("record instance %s deleting: %w", t.instanceID, err)
	}
	// The instance's desired_state is deleted, so its entry is not listed.
	if err := d.ensurePool(ctx, db, t); err != nil {
		return err
	}
	if err := job.Checkpoint(ctx, StepUnrouted); err != nil {
		return err
	}

	conn, err := d.connect(ctx, &t)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	role := pgx.Identifier{t.tenant.Name}.Sanitize()
	hasRole, err := roleExists(ctx, conn, t.tenant.Name)
	if err != nil {
		return err
	}
	var owner string
	err = conn.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, t.tenant.Name).Scan(&owner)
	hasDB := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("look up database %s: %w", t.tenant.Name, err)
	}
	if hasRole {
		if err := checkRoleAs(ctx, conn, t.tenant.Name, true); err != nil {
			return operations.Permanent("delete_failed", fmt.Errorf("%w: %w", ErrNotOurs, err))
		}
	}
	if hasDB && owner != t.tenant.Name {
		return operations.Permanent("delete_failed", fmt.Errorf("%w: database %s is owned by %s", ErrNotOurs, t.tenant.Name, owner))
	}

	if hasRole {
		if _, err := conn.Exec(ctx, "ALTER ROLE "+role+" NOLOGIN"); err != nil {
			return fmt.Errorf("close role %s: %w", t.tenant.Name, err)
		}
		if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, t.tenant.Name); err != nil {
			return fmt.Errorf("end sessions of %s: %w", t.tenant.Name, err)
		}
	}
	if err := job.Checkpoint(ctx, StepClosed); err != nil {
		return err
	}

	if hasDB {
		if err := d.finalDump(ctx, db, job, t); err != nil {
			return err
		}
	}
	if err := job.Checkpoint(ctx, StepDumped); err != nil {
		return err
	}

	if hasDB {
		if _, err := conn.Exec(ctx, "DROP DATABASE "+role+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop database %s: %w", t.tenant.Name, err)
		}
	}
	if hasRole {
		if _, err := conn.Exec(ctx, "DROP ROLE "+role); err != nil {
			return fmt.Errorf("drop role %s: %w", t.tenant.Name, err)
		}
	}
	if err := job.Checkpoint(ctx, StepDropped); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `UPDATE instances SET status = 'deleted', deleted_at = now(), reason = NULL
		WHERE id = $1 AND status <> 'deleted'`, t.instanceID); err != nil {
		return fmt.Errorf("record instance %s deleted: %w", t.instanceID, err)
	}
	return nil
}

// finalDump makes sure this operation's final dump exists: the one an
// earlier run recorded, if its file is intact, or a new one.
func (d *Driver) finalDump(ctx context.Context, db operations.DB, job *operations.Job, t target) error {
	var prev Dump
	err := db.QueryRow(ctx, `SELECT ref, size_bytes, sha256 FROM backup_records
		WHERE operation_id = $1 AND kind = 'final_dump' AND status = 'ok'`, job.ID).Scan(&prev.File, &prev.Size, &prev.SHA256)
	switch {
	case err == nil:
		if err := d.Dumper.Check(prev); err != nil {
			// Recorded but gone or changed: nothing may be dropped on it.
			return operations.Permanent("delete_failed", fmt.Errorf("the final dump an earlier run recorded is not intact: %w", err))
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("look up the final dump: %w", err)
	}
	dump, err := d.Dumper.Dump(ctx, t.cluster, t.tenant.Name, fmt.Sprintf("%s-%s.dump", t.tenant.Name, job.ID))
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO backup_records (instance_id, node_id, operation_id, kind, target, ref, size_bytes, sha256, status, expires_at)
		VALUES ($1, $2, $3, 'final_dump', 'local', $4, $5, $6, 'ok', now() + $7 * interval '1 second')`,
		t.instanceID, job.NodeID, job.ID, dump.File, dump.Size, dump.SHA256, UserDeleteRetention.Seconds()); err != nil {
		return fmt.Errorf("record the final dump: %w", err)
	}
	return nil
}
