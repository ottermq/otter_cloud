package operations

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the worker's Prometheus metrics. A failed run is what the
// "operation failed" alert watches (architecture.md §9).
type Metrics struct {
	runs *prometheus.CounterVec
}

// NewMetrics registers the worker's metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{runs: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "otter_operation_runs_total",
		Help: "Runs of an operation by this worker, by kind and outcome: succeeded, failed (for good), retried, lease_lost.",
	}, []string{"kind", "outcome"})}
	reg.MustRegister(m.runs)
	return m
}

func (m *Metrics) ran(kind, outcome string) {
	if m != nil {
		m.runs.WithLabelValues(kind, outcome).Inc()
	}
}
