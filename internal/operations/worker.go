package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// Handler runs one operation of a kind. Every step it takes is an ensure,
// so running it again after a crash or a retry is safe (architecture.md
// §5); job.Checkpoint records each step done. It returns nil when the
// operation succeeded, Permanent(…) when retrying won't help, and any other
// error to be retried with backoff. A handler that succeeds must also have
// recorded the outcome on the instance: the worker only finishes the
// operation row.
type Handler func(ctx context.Context, job *Job) error

// Job is a claimed operation as its handler sees it.
type Job struct {
	Op
	db DB
}

// DB is the platform DB as the node's worker, for the handler's reads and
// for recording the outcome on the instance.
func (j *Job) DB() DB { return j.db }

// Checkpoint records step as done.
func (j *Job) Checkpoint(ctx context.Context, step string) error {
	return Checkpoint(ctx, j.db, &j.Op, step)
}

// Permanent marks err as not worth retrying: the operation fails now, with
// the customer-safe reason.
func Permanent(reason string, err error) error { return &permanentError{reason, err} }

type permanentError struct {
	reason string
	err    error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Worker claims and runs its node's operations, one at a time.
type Worker struct {
	DB       DB
	Name     string             // in locked_by: the host and process
	Handlers map[string]Handler // by kind; only these kinds are claimed
	Lease    time.Duration      // DefaultLease when zero
	Poll     time.Duration      // how often to look without a notification; 2 s when zero
	Log      *slog.Logger
	Metrics  *Metrics // none when nil
	// Reconcile runs at startup and every ReconcileEvery
	// (DefaultReconcileEvery when zero), between operations, so it never
	// races one of this worker's. None when nil.
	Reconcile      Reconciler
	ReconcileEvery time.Duration
	reconciled     time.Time
}

// RunOnce claims one due operation and runs it to an outcome. It reports
// false when nothing was due.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	op, ok, err := Claim(ctx, w.DB, w.Name, slices.Sorted(maps.Keys(w.Handlers)), w.lease())
	if err != nil || !ok {
		return false, err
	}
	log := w.log().With("operation", op.ID, "kind", op.Kind, "attempt", op.Attempts)

	// Claimed again after its lease ran out, every time: the worker keeps
	// dying on it. Stop before running it once more.
	if op.Attempts > MaxAttempts {
		log.Error("operation failed: out of attempts")
		w.Metrics.ran(op.Kind, "failed")
		return true, Fail(ctx, w.DB, op, FailReason(op.Kind), fmt.Errorf("lease expired on each of %d attempts", MaxAttempts))
	}

	runErr, lost := w.run(ctx, op)
	switch {
	case lost:
		log.Warn("lease lost; another worker has the operation", "err", runErr)
		w.Metrics.ran(op.Kind, "lease_lost")
		return true, nil
	case ctx.Err() != nil:
		// Shutting down: the lease runs out and the operation is claimed
		// again, this attempt counted.
		return true, ctx.Err()
	case runErr == nil:
		log.Info("operation succeeded")
		w.Metrics.ran(op.Kind, "succeeded")
		return true, ignoreLost(Succeed(ctx, w.DB, op))
	}
	if p, ok := errors.AsType[*permanentError](runErr); ok {
		log.Error("operation failed", "reason", p.reason, "err", runErr)
		w.Metrics.ran(op.Kind, "failed")
		return true, ignoreLost(Fail(ctx, w.DB, op, p.reason, runErr))
	}
	failed, err := Retry(ctx, w.DB, op, runErr)
	if failed {
		log.Error("operation failed: out of attempts", "err", runErr)
		w.Metrics.ran(op.Kind, "failed")
	} else {
		log.Warn("operation will be retried", "err", runErr, "after", Backoff(op.Attempts))
		w.Metrics.ran(op.Kind, "retried")
	}
	return true, ignoreLost(err)
}

// run runs the handler while renewing the lease. lost reports that the
// lease was lost on the way, in which case the handler was canceled.
func (w *Worker) run(ctx context.Context, op Op) (err error, lost bool) {
	hctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{})
	renewed := make(chan bool, 1)
	go func() {
		t := time.NewTicker(w.lease() / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				renewed <- true
				return
			case <-t.C:
				if err := Renew(ctx, w.DB, op, w.lease()); errors.Is(err, ErrLeaseLost) {
					cancel(err)
					<-done
					renewed <- false
					return
				} else if err != nil {
					w.log().Warn("renew lease", "operation", op.ID, "err", err)
				}
			}
		}
	}()
	err = w.call(hctx, op)
	close(done)
	return err, !<-renewed
}

func (w *Worker) call(ctx context.Context, op Op) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	// Every JailDeck call the handler makes carries the operation id, so
	// JailDeck's audit log lines up with this one (JD-016).
	return w.Handlers[op.Kind](jaildeck.WithRequestID(ctx, op.ID), &Job{Op: op, db: w.DB})
}

// Run runs operations until ctx ends. listen is a connection of its own,
// as the same role, on which it LISTENs for new work; without a
// notification it looks every Poll.
func (w *Worker) Run(ctx context.Context, listen *pgx.Conn) error {
	node, err := NodeID(ctx, listen)
	if err != nil {
		return err
	}
	if _, err := listen.Exec(ctx, "LISTEN "+pgx.Identifier{Channel(node)}.Sanitize()); err != nil {
		return fmt.Errorf("listen for operations: %w", err)
	}
	w.log().Info("worker running", "node", node, "kinds", slices.Sorted(maps.Keys(w.Handlers)))
	for {
		w.reconcile(ctx, time.Now())
		if ctx.Err() != nil {
			return nil
		}
		ran, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.log().Error("run operation", "err", err)
		}
		if ran && err == nil {
			continue // there may be more due
		}
		wctx, cancel := context.WithTimeout(ctx, w.poll())
		_, err = listen.WaitForNotification(wctx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("wait for operations: %w", err)
		}
	}
}

func ignoreLost(err error) error {
	if errors.Is(err, ErrLeaseLost) {
		return nil
	}
	return err
}

func (w *Worker) lease() time.Duration {
	if w.Lease > 0 {
		return w.Lease
	}
	return DefaultLease
}

func (w *Worker) poll() time.Duration {
	if w.Poll > 0 {
		return w.Poll
	}
	return 2 * time.Second
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
