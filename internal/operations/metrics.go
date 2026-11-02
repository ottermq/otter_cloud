package operations

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the worker's Prometheus metrics. A failed run is what the
// "operation failed" alert watches (architecture.md §9).
type Metrics struct {
	runs, findings, reconciles *prometheus.CounterVec
	up                         *prometheus.GaugeVec
	mu                         sync.Mutex
	upSeries                   map[[2]string]bool // cluster, database
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
		}, []string{"outcome"}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "otter_instance_up",
			Help: "1 when a running database took a connection at its last health check, at PostgreSQL and at PgBouncer; 0 when it didn't.",
		}, []string{"cluster", "database"})}
	reg.MustRegister(m.runs, m.findings, m.reconciles, m.up)
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

// health sets otter_instance_up from a full pass, and drops the series
// of a database no longer checked (stopped, deleted).
func (m *Metrics) health(hs []Health) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[[2]string]bool{}
	for _, h := range hs {
		k := [2]string{h.Cluster, h.Database}
		seen[k] = true
		v := 0.0
		if h.Up {
			v = 1
		}
		m.up.WithLabelValues(k[0], k[1]).Set(v)
	}
	for k := range m.upSeries {
		if !seen[k] {
			m.up.DeleteLabelValues(k[0], k[1])
		}
	}
	m.upSeries = seen
}

func (m *Metrics) ran(kind, outcome string) {
	if m != nil {
		m.runs.WithLabelValues(kind, outcome).Inc()
	}
}
