package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// SnapshotPrefix starts the name of every snapshot the worker takes. The
// worker's JailDeck token may take and destroy only snapshots whose names
// match otter-* (JD-057), so it can't touch the replication tool's or the
// operator's.
const SnapshotPrefix = "otter-snapshot-"

// SnapshotBacker returns the node's cluster snapshots for operations.Worker
// (#116): each cluster on the node is snapshotted every, and each snapshot
// kept keep. every zero is off: the node's replication tool snapshots the
// jails' datasets instead (architecture.md §10).
func (d *Driver) SnapshotBacker(db operations.DB, every, keep time.Duration) operations.Backer {
	return func(ctx context.Context, now time.Time) ([]operations.Backup, error) {
		if every <= 0 {
			return nil, nil
		}
		return d.SnapshotCluster(ctx, db, now, every, keep)
	}
}

// SnapshotCluster snapshots one cluster on the node that is due: none of
// its snapshots, taken or tried, is younger than every. The one whose last
// snapshot is oldest goes first. A snapshot takes the cluster jail's root
// and its data at one point in time: crash-consistent, as after a power
// loss, which PostgreSQL recovers from.
//
// Each attempt is recorded, a failed one too, so a cluster that fails is
// tried again at its next turn, not in a loop; the failure is the error,
// for the log and the alert on otter_backups_total.
func (d *Driver) SnapshotCluster(ctx context.Context, db operations.DB, now time.Time, every, keep time.Duration) ([]operations.Backup, error) {
	var id, cluster, jail string
	err := db.QueryRow(ctx, `
		SELECT c.id, c.name, c.jail_name FROM clusters c
		WHERE c.status <> 'offline'
		  AND NOT EXISTS (SELECT FROM backup_records b
			WHERE b.cluster_id = c.id AND b.kind = 'snapshot' AND b.created_at > $1)
		ORDER BY (SELECT max(b.created_at) FROM backup_records b
			WHERE b.cluster_id = c.id AND b.kind = 'snapshot') NULLS FIRST, c.id
		LIMIT 1`, now.Add(-every)).Scan(&id, &cluster, &jail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find a cluster due its snapshot: %w", err)
	}
	name := SnapshotPrefix + now.UTC().Format("20060102T150405Z")
	b := operations.Backup{Kind: "snapshot", Cluster: cluster, Status: "ok"}
	res, err := d.JailDeck.Snapshot(ctx, jail, name)
	if err != nil {
		b.Status = "failed"
		if _, rerr := db.Exec(ctx, `
			INSERT INTO backup_records (cluster_id, node_id, kind, target, ref, status, created_at)
			SELECT id, node_id, 'snapshot', 'local', $2, 'failed', $3 FROM clusters WHERE id = $1`, id, name, now); rerr != nil {
			return nil, errors.Join(fmt.Errorf("snapshot %s: %w", cluster, err), fmt.Errorf("record the failed snapshot: %w", rerr))
		}
		return []operations.Backup{b}, fmt.Errorf("snapshot %s: %w", cluster, err)
	}
	// ref is the full dataset@name, which the destroy must confirm.
	if _, err := db.Exec(ctx, `
		INSERT INTO backup_records (cluster_id, node_id, kind, target, ref, status, created_at, expires_at)
		SELECT id, node_id, 'snapshot', 'local', $2, 'ok', $3::timestamptz, $3::timestamptz + $4 * interval '1 second'
		FROM clusters WHERE id = $1`, id, res.Snapshot, now, keep.Seconds()); err != nil {
		return []operations.Backup{b}, fmt.Errorf("record the snapshot of %s: %w", cluster, err)
	}
	return []operations.Backup{b}, nil
}

// SnapshotExpirer returns the node's snapshot retention for
// operations.Worker. It runs whether the schedule is on or not, so turning
// it off still lets the snapshots taken expire.
func (d *Driver) SnapshotExpirer(db operations.TxDB, on bool) operations.Expirer {
	return func(ctx context.Context, now time.Time) ([]operations.Expiry, error) {
		return d.PurgeSnapshots(ctx, db, now, on)
	}
}

// PurgeSnapshots destroys the node's cluster snapshots whose expires_at is
// past, as of now, and marks each purged, with its audit event. While the
// schedule is on, a cluster's newest snapshot is kept however old, as a
// database's newest dump is: if snapshots start failing, the last good one
// stays. A snapshot already gone is not an error; one JailDeck refuses (a
// hold, or a clone made from it) is, and is tried again at the next run,
// without keeping the others from going.
func (d *Driver) PurgeSnapshots(ctx context.Context, db operations.TxDB, now time.Time, keepNewest bool) ([]operations.Expiry, error) {
	rows, err := db.Query(ctx, `
		SELECT b.id, b.ref, c.name, c.jail_name
		FROM backup_records b JOIN clusters c ON c.id = b.cluster_id
		WHERE b.kind = 'snapshot' AND b.status = 'ok' AND b.purged_at IS NULL AND b.expires_at <= $1
		  AND NOT ($2 AND b.id = (
			SELECT n.id FROM backup_records n
			WHERE n.cluster_id = b.cluster_id AND n.kind = 'snapshot' AND n.status = 'ok'
			ORDER BY n.created_at DESC, n.id DESC LIMIT 1))
		ORDER BY b.expires_at, b.id`, now, keepNewest)
	if err != nil {
		return nil, fmt.Errorf("list expired snapshots: %w", err)
	}
	type snapshot struct{ id, ref, cluster, jail string }
	snaps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (snapshot, error) {
		var x snapshot
		return x, r.Scan(&x.id, &x.ref, &x.cluster, &x.jail)
	})
	if err != nil {
		return nil, fmt.Errorf("list expired snapshots: %w", err)
	}
	var done []operations.Expiry
	var errs []error
	for _, x := range snaps {
		_, short, ok := strings.Cut(x.ref, "@")
		if !ok || !strings.HasPrefix(short, SnapshotPrefix) {
			errs = append(errs, fmt.Errorf("snapshot %s: %q is not one of the worker's", x.id, x.ref))
			continue
		}
		gone := false
		if _, err := d.JailDeck.DestroySnapshot(ctx, x.jail, short, x.ref); jaildeck.IsNotFound(err) {
			gone = true
		} else if err != nil {
			errs = append(errs, fmt.Errorf("destroy snapshot %s: %w", x.ref, err))
			continue
		}
		if err := markPurged(ctx, db, x.id, "", map[string]any{"snapshot": x.ref, "cluster": x.cluster}, gone); err != nil {
			return done, errors.Join(append(errs, err)...)
		}
		done = append(done, operations.Expiry{Action: "purge", Subject: x.id})
	}
	return done, errors.Join(errs...)
}
