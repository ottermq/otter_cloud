package operations

import (
	"context"
	"time"
)

// Finding is one thing the reconciler saw that the platform DB doesn't
// account for, and what it did about it (architecture.md §5). The worker
// logs each as an error and counts it: that is the alert.
type Finding struct {
	// database_missing, orphan_queued, orphan_delete_failed,
	// unknown_database, pool_repaired, cluster_unreachable
	Kind    string
	Cluster string
	Subject string // the database or entry; never a customer's name
	Detail  string
}

// Reconciler compares what exists on the node with the platform DB's rows,
// fixes what it safely can, and reports the rest. It never destroys
// anything it doesn't know.
type Reconciler func(ctx context.Context) ([]Finding, error)

// DefaultReconcileEvery is how often the worker reconciles, besides at
// startup.
const DefaultReconcileEvery = 5 * time.Minute

// reconcile runs the reconciler if it is due.
func (w *Worker) reconcile(ctx context.Context, now time.Time) {
	if w.Reconcile == nil || (!w.reconciled.IsZero() && now.Sub(w.reconciled) < w.reconcileEvery()) {
		return
	}
	w.reconciled = now
	findings, err := w.Reconcile(ctx)
	for _, f := range findings {
		w.log().Error("reconcile: "+f.Kind, "cluster", f.Cluster, "subject", f.Subject, "detail", f.Detail)
		w.Metrics.found(f.Kind)
	}
	if err != nil {
		w.log().Error("reconcile", "err", err)
		w.Metrics.reconciled("failed")
		return
	}
	w.Metrics.reconciled("ok")
}

func (w *Worker) reconcileEvery() time.Duration {
	if w.ReconcileEvery > 0 {
		return w.ReconcileEvery
	}
	return DefaultReconcileEvery
}
