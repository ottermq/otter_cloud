package operations

import (
	"context"
	"time"
)

// Storage is one database's storage check (Q27, #99): its size against its
// allocation, and the step it is in.
type Storage struct {
	InstanceID string
	Cluster    string
	Database   string // the infra name; never a customer's name
	Used       int64  // bytes, pg_database_size()
	Allocated  int64  // bytes
	From, To   string // its storage_state before and after this check
}

// StorageChecker measures the node's databases, moves each through the
// steps of Q27, and enforces the step on the cluster.
type StorageChecker func(ctx context.Context, now time.Time) ([]Storage, error)

// DefaultStorageEvery is how often the worker checks storage, besides at
// startup.
const DefaultStorageEvery = time.Minute

// checkStorage runs the storage checker if it is due. A database entering
// read_only is a warning line, and blocked an error: the operator's alert.
func (w *Worker) checkStorage(ctx context.Context, now time.Time) {
	if w.CheckStorage == nil || (!w.storageChecked.IsZero() && now.Sub(w.storageChecked) < w.storageEvery()) {
		return
	}
	w.storageChecked = now
	ss, err := w.CheckStorage(ctx, now)
	for _, s := range ss {
		if s.From == s.To {
			continue
		}
		args := []any{"instance", s.InstanceID, "cluster", s.Cluster, "database", s.Database,
			"from", s.From, "to", s.To, "used_bytes", s.Used, "allocated_bytes", s.Allocated}
		switch s.To {
		case "blocked":
			w.log().Error("storage: blocked", args...)
		case "read_only":
			w.log().Warn("storage: read_only", args...)
		default:
			w.log().Info("storage: "+s.To, args...)
		}
	}
	if err != nil {
		w.log().Error("storage check", "err", err)
		return
	}
	w.Metrics.storage(ss)
}

func (w *Worker) storageEvery() time.Duration {
	if w.StorageEvery > 0 {
		return w.StorageEvery
	}
	return DefaultStorageEvery
}
