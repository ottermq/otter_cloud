package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// Expirer returns the node's half of the expiry policy for
// operations.Worker (#100): the dumps past their expiry. db is the
// platform DB as the node's worker.
func (d *Driver) Expirer(db operations.TxDB) operations.Expirer {
	return func(ctx context.Context, now time.Time) ([]operations.Expiry, error) {
		return d.PurgeDumps(ctx, db, now)
	}
}

// PurgeDumps removes the node's dumps, final (Q26) and nightly (#117),
// whose expires_at is past, as of now, and marks each purged, with its
// audit event. The file goes first: if marking it fails, the next run finds
// the record again, and a file already gone is not an error. Only dumps on
// the node (target local) are the worker's to remove; a record of one that
// was never written isn't a dump. The newest nightly dump of a database
// not deleted is kept, however old: a database stopped for weeks takes no
// dumps, and its last one is all there is.
func (d *Driver) PurgeDumps(ctx context.Context, db operations.TxDB, now time.Time) ([]operations.Expiry, error) {
	rows, err := db.Query(ctx, `
		SELECT b.id, b.ref, coalesce(i.account_id::text, ''), b.instance_id
		FROM backup_records b LEFT JOIN instances i ON i.id = b.instance_id
		WHERE b.kind IN ('dump', 'final_dump') AND b.status = 'ok' AND b.target = 'local'
		  AND b.purged_at IS NULL AND b.expires_at <= $1
		  AND NOT (b.kind = 'dump' AND i.deleted_at IS NULL AND b.id = (
			SELECT n.id FROM backup_records n
			WHERE n.instance_id = b.instance_id AND n.kind = 'dump' AND n.status = 'ok'
			ORDER BY n.created_at DESC, n.id DESC LIMIT 1))
		ORDER BY b.expires_at, b.id`, now)
	if err != nil {
		return nil, fmt.Errorf("list expired dumps: %w", err)
	}
	type dump struct{ id, file, accountID, instanceID string }
	dumps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dump, error) {
		var x dump
		var instanceID *string
		err := r.Scan(&x.id, &x.file, &x.accountID, &instanceID)
		if instanceID != nil {
			x.instanceID = *instanceID
		}
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("list expired dumps: %w", err)
	}
	var done []operations.Expiry
	for _, x := range dumps {
		gone, err := d.Dumper.Remove(x.file)
		if err != nil {
			return done, fmt.Errorf("remove dump %s: %w", x.id, err)
		}
		if err := markPurged(ctx, db, x.id, x.accountID, x.instanceID, x.file, gone); err != nil {
			return done, err
		}
		done = append(done, operations.Expiry{Action: "purge", Subject: x.id})
	}
	return done, nil
}

func markPurged(ctx context.Context, db operations.TxDB, id, accountID, instanceID, file string, gone bool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE backup_records SET purged_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark dump %s purged: %w", id, err)
	}
	details := map[string]any{"file": file, "instance_id": instanceID}
	if gone {
		details["already_gone"] = true
	}
	if err := audit.Record(ctx, tx, audit.Event{Actor: audit.System, AccountID: accountID, Action: audit.ActionBackupPurge,
		TargetType: "backup_record", TargetID: id, Outcome: audit.OK, Details: details}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
