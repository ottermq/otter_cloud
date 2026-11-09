package operations

import (
	"context"
	"time"
)

// DefaultBackupEvery is how often the worker looks for a database due a
// backup, besides at startup. After one, it looks again straight away.
const DefaultBackupEvery = time.Minute

// Backup is one backup the worker made, or tried to: a dump on its node
// (#117), or a copy of one on its target (#118).
type Backup struct {
	Kind       string // dump: a nightly dump; final_dump
	Target     string // local, on the node, when empty; else the target's name
	InstanceID string
	Database   string // the infra name; never a customer's name
	Status     string // ok or failed
	Size       int64  // bytes; 0 when failed
}

// Backer makes the node's next due backup, if one is due as of now.
type Backer func(ctx context.Context, now time.Time) ([]Backup, error)

// Backers is one Backer of several: the first that makes or tries a
// backup has the call, so a night's dumps come before their copies.
func Backers(bs ...Backer) Backer {
	return func(ctx context.Context, now time.Time) ([]Backup, error) {
		for _, b := range bs {
			if done, err := b(ctx, now); len(done) > 0 || err != nil {
				return done, err
			}
		}
		return nil, nil
	}
}

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
		target := b.Target
		if target == "" {
			target = "local"
		}
		args := []any{"instance", b.InstanceID, "database", b.Database, "target", target, "size_bytes", b.Size}
		if b.Status == "ok" {
			w.log().Info("backup: "+b.Kind, args...)
		}
		w.Metrics.backup(b.Kind, target, b.Status)
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
