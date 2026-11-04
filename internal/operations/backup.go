package operations

import (
	"context"
	"time"
)

// DefaultBackupEvery is how often the worker looks for a database due a
// backup, besides at startup. After one, it looks again straight away.
const DefaultBackupEvery = time.Minute

// Backup is one backup the worker made, or tried to, on its node (#117).
type Backup struct {
	Kind       string // dump: a nightly dump
	InstanceID string
	Database   string // the infra name; never a customer's name
	Status     string // ok or failed
	Size       int64  // bytes; 0 when failed
}

// Backer makes the node's next due backup, if one is due as of now.
type Backer func(ctx context.Context, now time.Time) ([]Backup, error)

// backup runs the backer if it is due. One that made a backup is due again
// at once, so a night's dumps follow each other between operations.
func (w *Worker) backup(ctx context.Context, now time.Time) {
	if w.Backup == nil || (!w.backedUp.IsZero() && now.Sub(w.backedUp) < w.backupEvery()) {
		return
	}
	done, err := w.Backup(ctx, now)
	if len(done) == 0 || err != nil {
		w.backedUp = now
	}
	for _, b := range done {
		args := []any{"instance", b.InstanceID, "database", b.Database, "size_bytes", b.Size}
		if b.Status == "ok" {
			w.log().Info("backup: "+b.Kind, args...)
		}
		w.Metrics.backup(b.Kind, b.Status)
	}
	if err != nil {
		w.log().Error("backup", "err", err)
	}
}

func (w *Worker) backupEvery() time.Duration {
	if w.BackupEvery > 0 {
		return w.BackupEvery
	}
	return DefaultBackupEvery
}
