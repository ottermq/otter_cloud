package operations

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the worker's Prometheus metrics. A failed run is what the
// "operation failed" alert watches (architecture.md §9).
type Metrics struct {
	runs, findings, reconciles *prometheus.CounterVec
}

// NewMetrics registers the worker's metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{runs: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "otter_operation_runs_total",
		Help: "Runs of an operation by this worker, by kind and outcome: succeeded, failed (for good), retried, lease_lost.",
	}, []string{"kind", "outcome"}),
		findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "otter_reconcile_findings_total",
			Help: "What the reconciler found that the platform DB doesn't account for, by kind. Every one is worth an alert.",
		}, []string{"kind"}),
		reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "otter_reconcile_runs_total",
			Help: "Reconciler runs by outcome: ok or failed.",
		}, []string{"outcome"})}
	reg.MustRegister(m.runs, m.findings, m.reconciles)
	return m
}

func (m *Metrics) found(kind string) {
	if m != nil {
		m.findings.WithLabelValues(kind).Inc()
	}
}

func (m *Metrics) reconciled(outcome string) {
	if m != nil {
		m.reconciles.WithLabelValues(outcome).Inc()
	}
}

func (m *Metrics) ran(kind, outcome string) {
	if m != nil {
		m.runs.WithLabelValues(kind, outcome).Inc()
	}
}
