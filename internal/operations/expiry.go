package operations

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultExpireEvery is how often the worker removes what has expired on
// its node, besides at startup.
const DefaultExpireEvery = time.Minute

// Expiry is one thing past its expiry that the worker removed from its
// node (#100).
type Expiry struct {
	Action  string // purge: a final dump
	Subject string // the backup record's id
}

// Expirer removes what has expired on the node as of now: the node's half
// of the expiry policy. Which instances expire is the control plane's
// (instances.Expire): it records intent, which a worker never does.
type Expirer func(ctx context.Context, now time.Time) ([]Expiry, error)

// TxDB is a DB that begins transactions.
type TxDB interface {
	DB
	Begin(ctx context.Context) (pgx.Tx, error)
}

// expire runs the expirer if it is due.
func (w *Worker) expire(ctx context.Context, now time.Time) {
	if w.Expire == nil || (!w.expired.IsZero() && now.Sub(w.expired) < w.expireEvery()) {
		return
	}
	w.expired = now
	done, err := w.Expire(ctx, now)
	for _, e := range done {
		w.log().Info("expiry: "+e.Action, "subject", e.Subject)
		w.Metrics.expiry(e.Action)
	}
	if err != nil {
		w.log().Error("expiry", "err", err)
	}
}

func (w *Worker) expireEvery() time.Duration {
	if w.ExpireEvery > 0 {
		return w.ExpireEvery
	}
	return DefaultExpireEvery
}
