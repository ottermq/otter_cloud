package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// RetryAfter is how long a copy that failed waits before it is tried again,
// so a target that is down doesn't hold up the worker.
const RetryAfter = time.Hour

// Shipper copies the node's dumps to its target, sealed, and removes the
// copies there when their records expire (#118). Each copy is a
// backup_records row of its own: the dump's kind, the target's name, and
// the dump it copies, whose expiry it shares.
//
// With no Target (none), each dump gets a skipped row instead, so the gap
// shows in the platform DB and in otter_backups_total, not only in the
// configuration.
type Shipper struct {
	// Name is the target's in backup_records; never "local".
	Name   string
	Target Target // none when nil
	Sealer Sealer
	// Dir is the node's dump directory, where the dumps are read and
	// sealed.
	Dir string
	// MaxBytes is the most the node's copies on the target may add up to;
	// 0, no limit. A copy that would go over it fails.
	MaxBytes int64
}

// Backer returns the shipping half of the node's backups for
// operations.Worker: db is the platform DB as the node's worker.
func (s *Shipper) Backer(db operations.DB) operations.Backer {
	return func(ctx context.Context, now time.Time) ([]operations.Backup, error) {
		return s.Ship(ctx, db, now)
	}
}

// Expirer returns the copies' half of the expiry policy for
// operations.Worker.
func (s *Shipper) Expirer(db operations.TxDB) operations.Expirer {
	return func(ctx context.Context, now time.Time) ([]operations.Expiry, error) {
		return s.Purge(ctx, db, now)
	}
}

// Key is where the copy of a dump goes on the target.
func Key(dump string) string { return "dumps/" + dump + ".age" }

// Ship copies one dump on the node that the target hasn't got (or records
// it skipped, with no target): a final dump first, then the oldest. A dump past its expiry isn't copied, nor one
// whose copy failed less than RetryAfter ago. Each attempt is recorded; the
// failure is also the error.
func (s *Shipper) Ship(ctx context.Context, db operations.DB, now time.Time) ([]operations.Backup, error) {
	var src struct {
		id, kind, instanceID, name, file, sha256 string
		size                                     int64
	}
	err := db.QueryRow(ctx, `
		SELECT b.id, b.kind, coalesce(b.instance_id::text, ''), coalesce(i.infra_name, ''), b.ref, b.size_bytes, b.sha256
		FROM backup_records b LEFT JOIN instances i ON i.id = b.instance_id
		WHERE b.target = 'local' AND b.status = 'ok' AND b.kind IN ('dump', 'final_dump')
		  AND b.purged_at IS NULL AND (b.expires_at IS NULL OR b.expires_at > $2)
		  AND NOT EXISTS (SELECT FROM backup_records c WHERE c.source_id = b.id AND c.target = $1
			AND (c.status IN ('ok', 'skipped') OR c.created_at > $3))
		ORDER BY b.kind = 'final_dump' DESC, b.created_at, b.id
		LIMIT 1`, s.Name, now, now.Add(-RetryAfter)).Scan(
		&src.id, &src.kind, &src.instanceID, &src.name, &src.file, &src.size, &src.sha256)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find a dump to copy to %s: %w", s.Name, err)
	}
	key := Key(src.file)
	b := operations.Backup{Kind: src.kind, Target: s.Name, InstanceID: src.instanceID, Database: src.name, Status: "ok"}
	if s.Target == nil {
		b.Status = "skipped"
		if _, err := db.Exec(ctx, `
			INSERT INTO backup_records (instance_id, node_id, kind, target, ref, status, created_at, source_id)
			SELECT instance_id, node_id, kind, $2, $3, 'skipped', $4, id FROM backup_records WHERE id = $1`,
			src.id, s.Name, key, now); err != nil {
			return nil, fmt.Errorf("record %s not copied: %w", src.file, err)
		}
		return []operations.Backup{b}, nil
	}
	size, sum, err := s.copy(ctx, db, src.file, src.size, src.sha256, key)
	if err != nil {
		b.Status = "failed"
		if _, rerr := db.Exec(ctx, `
			INSERT INTO backup_records (instance_id, node_id, kind, target, ref, status, created_at, source_id)
			SELECT instance_id, node_id, kind, $2, $3, 'failed', $4, id FROM backup_records WHERE id = $1`,
			src.id, s.Name, key, now); rerr != nil {
			return nil, errors.Join(fmt.Errorf("copy %s to %s: %w", src.file, s.Name, err), fmt.Errorf("record the failed copy: %w", rerr))
		}
		return []operations.Backup{b}, fmt.Errorf("copy %s to %s: %w", src.file, s.Name, err)
	}
	b.Size = size
	// The copy's expiry is the dump's as it stands now: a delete that
	// shortened the dump's has shortened the copy's too.
	if _, err := db.Exec(ctx, `
		INSERT INTO backup_records (instance_id, node_id, kind, target, ref, size_bytes, sha256, status, created_at, expires_at, source_id)
		SELECT instance_id, node_id, kind, $2, $3, $4, $5, 'ok', $6, expires_at, id FROM backup_records WHERE id = $1`,
		src.id, s.Name, key, size, sum, now); err != nil {
		return []operations.Backup{b}, fmt.Errorf("record the copy of %s: %w", src.file, err)
	}
	return []operations.Backup{b}, nil
}

// copy seals the dump file and puts it on the target, if it is still what
// was recorded and fits under MaxBytes.
func (s *Shipper) copy(ctx context.Context, db operations.DB, file string, size int64, sum, key string) (int64, string, error) {
	f, err := os.Open(filepath.Join(s.Dir, file))
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	sealed, err := s.Sealer.Seal(f, s.Dir)
	if err != nil {
		return 0, "", err
	}
	defer sealed.Close()
	if sealed.PlainSize != size || sealed.PlainSHA256 != sum {
		return 0, "", fmt.Errorf("the dump is not what was recorded: %d bytes", sealed.PlainSize)
	}
	if s.MaxBytes > 0 {
		var held int64
		if err := db.QueryRow(ctx, `
			SELECT coalesce(sum(size_bytes), 0) FROM backup_records
			WHERE target = $1 AND status = 'ok' AND purged_at IS NULL`, s.Name).Scan(&held); err != nil {
			return 0, "", err
		}
		if held+sealed.Size > s.MaxBytes {
			return 0, "", fmt.Errorf("%d bytes would take the copies to %d, over the %d allowed", sealed.Size, held+sealed.Size, s.MaxBytes)
		}
	}
	if err := s.Target.Put(ctx, key, sealed.Blob); err != nil {
		return 0, "", err
	}
	return sealed.Size, sealed.SHA256, nil
}

// Purge removes the node's copies on the target whose expires_at is past,
// as of now, and marks each purged, with its audit event: as PurgeDumps
// does for the dumps on the node, the newest copy of a nightly dump of a
// database not deleted is kept, however old.
func (s *Shipper) Purge(ctx context.Context, db operations.TxDB, now time.Time) ([]operations.Expiry, error) {
	if s.Target == nil {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		SELECT b.id, b.ref, coalesce(i.account_id::text, ''), coalesce(b.instance_id::text, '')
		FROM backup_records b LEFT JOIN instances i ON i.id = b.instance_id
		WHERE b.target = $1 AND b.status = 'ok' AND b.purged_at IS NULL AND b.expires_at <= $2
		  AND NOT (b.kind = 'dump' AND i.deleted_at IS NULL AND b.id = (
			SELECT n.id FROM backup_records n
			WHERE n.instance_id = b.instance_id AND n.kind = 'dump' AND n.status = 'ok' AND n.target = b.target
			ORDER BY n.created_at DESC, n.id DESC LIMIT 1))
		ORDER BY b.expires_at, b.id`, s.Name, now)
	if err != nil {
		return nil, fmt.Errorf("list expired copies on %s: %w", s.Name, err)
	}
	type copy struct{ id, key, accountID, instanceID string }
	copies, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (copy, error) {
		var c copy
		return c, r.Scan(&c.id, &c.key, &c.accountID, &c.instanceID)
	})
	if err != nil {
		return nil, fmt.Errorf("list expired copies on %s: %w", s.Name, err)
	}
	var done []operations.Expiry
	for _, c := range copies {
		if err := s.Target.Delete(ctx, c.key); err != nil {
			return done, fmt.Errorf("remove copy %s from %s: %w", c.id, s.Name, err)
		}
		if err := s.markPurged(ctx, db, c.id, c.accountID, c.instanceID, c.key); err != nil {
			return done, err
		}
		done = append(done, operations.Expiry{Action: "purge", Subject: c.id})
	}
	return done, nil
}

func (s *Shipper) markPurged(ctx context.Context, db operations.TxDB, id, accountID, instanceID, key string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE backup_records SET purged_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark copy %s purged: %w", id, err)
	}
	if err := audit.Record(ctx, tx, audit.Event{Actor: audit.System, AccountID: accountID, Action: audit.ActionBackupPurge,
		TargetType: "backup_record", TargetID: id, Outcome: audit.OK,
		Details: map[string]any{"target": s.Name, "key": key, "instance_id": instanceID}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
