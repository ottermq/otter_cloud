package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/operations"
)

// How long a nightly dump is kept (architecture.md §10): 7 days, and 28 for
// the first of each week, so a week of daily dumps and four weekly ones are
// on hand. A delete shortens them (Delete).
const (
	DailyDumpRetention  = 7 * 24 * time.Hour
	WeeklyDumpRetention = 28 * 24 * time.Hour
)

// DumpHour is the hour, in the node's local time, from which each running
// database is dumped once a day.
const DumpHour = 3

// DumpDueSince is the latest DumpHour at or before now, in now's location:
// a database with no dump attempt since is due one.
func DumpDueSince(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), DumpHour, 0, 0, 0, now.Location())
	if now.Before(t) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// Backer returns the node's nightly dumps for operations.Worker: db is the
// platform DB as the node's worker.
func (d *Driver) Backer(db operations.DB) operations.Backer {
	return func(ctx context.Context, now time.Time) ([]operations.Backup, error) {
		return d.NightlyDump(ctx, db, now)
	}
}

// NightlyDump dumps one database on the node that is due (#117): running,
// not blocked by its storage, and with no dump attempt since
// DumpDueSince(now). The one whose last dump is oldest goes first. A
// stopped database refuses connections, and its data can't change: its
// last dump stays, as PurgeDumps never removes a database's newest.
//
// Each attempt is recorded, a failed one too, so a database that fails is
// tried again the next night, not in a loop that starves the others; the
// failure is the error, for the log and the alert on otter_backups_total.
func (d *Driver) NightlyDump(ctx context.Context, db operations.DB, now time.Time) ([]operations.Backup, error) {
	var id, name string
	err := db.QueryRow(ctx, `
		SELECT i.id, i.infra_name FROM instances i
		WHERE i.status = 'running' AND i.storage_state <> 'blocked'
		  AND NOT EXISTS (SELECT FROM backup_records b
			WHERE b.instance_id = i.id AND b.kind = 'dump' AND b.created_at >= $1)
		ORDER BY (SELECT max(b.created_at) FROM backup_records b
			WHERE b.instance_id = i.id AND b.kind = 'dump') NULLS FIRST, i.id
		LIMIT 1`, DumpDueSince(now)).Scan(&id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find a database due its dump: %w", err)
	}
	file := fmt.Sprintf("%s-%s.dump", name, now.UTC().Format("20060102T150405Z"))
	b := operations.Backup{Kind: "dump", InstanceID: id, Database: name, Status: "ok"}
	dump, err := d.nightlyDump(ctx, db, id, file)
	if err != nil {
		b.Status = "failed"
		if _, rerr := db.Exec(ctx, `
			INSERT INTO backup_records (instance_id, node_id, kind, target, ref, status, created_at)
			SELECT id, node_id, 'dump', 'local', $2, 'failed', $3 FROM instances WHERE id = $1`, id, file, now); rerr != nil {
			return nil, errors.Join(fmt.Errorf("dump %s: %w", name, err), fmt.Errorf("record the failed dump: %w", rerr))
		}
		return []operations.Backup{b}, fmt.Errorf("dump %s: %w", name, err)
	}
	b.Size = dump.Size
	// The first dump in a week is a weekly one. Six and a half days, not
	// seven, so that a dump made a little earlier in the night than last
	// week's is still the week's. Its times are now's, the clock the
	// schedule reads.
	if _, err := db.Exec(ctx, `
		INSERT INTO backup_records (instance_id, node_id, kind, target, ref, size_bytes, sha256, status, created_at, expires_at)
		SELECT i.id, i.node_id, 'dump', 'local', $2, $3, $4, 'ok', $7::timestamptz, $7::timestamptz + CASE WHEN EXISTS (
			SELECT FROM backup_records w
			WHERE w.instance_id = i.id AND w.kind = 'dump' AND w.status = 'ok'
			  AND w.created_at > $7::timestamptz - interval '6 days 12 hours'
			  AND w.expires_at - w.created_at > $5 * interval '1 second')
			THEN $5 ELSE $6 END * interval '1 second'
		FROM instances i WHERE i.id = $1`,
		id, dump.File, dump.Size, dump.SHA256, DailyDumpRetention.Seconds(), WeeklyDumpRetention.Seconds(), now); err != nil {
		return []operations.Backup{b}, fmt.Errorf("record the dump of %s: %w", name, err)
	}
	return []operations.Backup{b}, nil
}

func (d *Driver) nightlyDump(ctx context.Context, db operations.DB, id, file string) (Dump, error) {
	t, err := d.load(ctx, db, id, "")
	if err != nil {
		return Dump{}, err
	}
	if t.cluster.ServerCert, err = ServerCert(ctx, d.JailDeck, t.jail, t.pgMajor); err != nil {
		return Dump{}, err
	}
	return d.Dumper.Dump(ctx, t.cluster, t.tenant.Name, file)
}

// shortenDumps makes the instance's nightly dumps expire no later than keep
// from now, at its delete: none outlives the final dump, and an ephemeral
// database's go at once (keep 0).
func shortenDumps(ctx context.Context, db operations.DB, instanceID string, keep time.Duration) error {
	if _, err := db.Exec(ctx, `
		UPDATE backup_records SET expires_at = least(expires_at, now() + $2 * interval '1 second')
		WHERE instance_id = $1 AND kind = 'dump' AND status = 'ok' AND purged_at IS NULL`,
		instanceID, keep.Seconds()); err != nil {
		return fmt.Errorf("shorten the nightly dumps of %s: %w", instanceID, err)
	}
	return nil
}
