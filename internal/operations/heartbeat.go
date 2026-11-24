package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// NetworkChecker reads the host's tenant networks from JailDeck.
type NetworkChecker interface {
	Networks(ctx context.Context) (jaildeck.Networks, error)
}

// DefaultHeartbeatEvery is how often the worker records that it is alive,
// besides at startup.
const DefaultHeartbeatEvery = 30 * time.Second

// LastBackup is when the node's last successful backup of a kind, to a
// target, was made.
type LastBackup struct {
	Kind, Target string
	At           time.Time
}

// Heartbeat records on the node's row that its worker is alive and reaches
// the platform DB (#200), and reads when each kind of the node's backups
// last succeeded. The control plane reads last_seen_at, so a node whose
// worker is down, or can't reach the platform DB, shows from off the node.
func Heartbeat(ctx context.Context, db DB) ([]LastBackup, error) {
	// The worker's policy limits both statements to its own node.
	tag, err := db.Exec(ctx, `UPDATE infrastructure_nodes SET last_seen_at = now() WHERE id = worker_node_id()`)
	if err != nil {
		return nil, fmt.Errorf("record the heartbeat: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, errors.New("record the heartbeat: this role is no node's worker")
	}
	rows, err := db.Query(ctx, `
		SELECT kind, target, max(created_at) FROM backup_records
		WHERE status = 'ok' GROUP BY kind, target ORDER BY kind, target`)
	if err != nil {
		return nil, fmt.Errorf("read the last backups: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LastBackup, error) {
		var b LastBackup
		return b, r.Scan(&b.Kind, &b.Target, &b.At)
	})
}

// heartbeat records the heartbeat if it is due. Its outcome is
// otter_worker_platform_db_up; the last backups it read are
// otter_backup_last_success_timestamp_seconds. It also asks JailDeck for
// the host's networks: whether JailDeck answers, and how many networks
// it has lost, are what nothing off the node could otherwise see (#200).
func (w *Worker) heartbeat(ctx context.Context, now time.Time) {
	if !w.beat.IsZero() && now.Sub(w.beat) < w.heartbeatEvery() {
		return
	}
	w.beat = now
	w.checkJailDeck(ctx)
	last, err := Heartbeat(ctx, w.DB)
	if err != nil {
		if ctx.Err() == nil {
			w.log().Error("heartbeat", "err", err)
			w.Metrics.PlatformDB(false)
		}
		return
	}
	w.Metrics.lastBackups(last)
	w.Metrics.PlatformDB(true)
}

func (w *Worker) checkJailDeck(ctx context.Context) {
	if w.JailDeck == nil {
		return
	}
	n, err := w.JailDeck.Networks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log().Error("jaildeck check", "err", err)
			w.Metrics.jailDeck(false, 0)
		}
		return
	}
	if len(n.Missing) > 0 {
		names := make([]string, len(n.Missing))
		for i, m := range n.Missing {
			names[i] = m.Name
		}
		w.log().Error("jaildeck check: networks missing", "networks", names)
	}
	w.Metrics.jailDeck(true, len(n.Missing))
}

func (w *Worker) heartbeatEvery() time.Duration {
	if w.HeartbeatEvery > 0 {
		return w.HeartbeatEvery
	}
	return DefaultHeartbeatEvery
}
