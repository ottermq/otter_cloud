package operations

import (
	"context"
	"time"
)

// Health is one instance's health check: whether its database takes
// connections, at PostgreSQL and at PgBouncer (#97).
type Health struct {
	InstanceID string
	Cluster    string
	Database   string // the infra name; never a customer's name
	Up         bool
	Changed    bool   // the instance's health was recorded anew
	Detail     string // why it is down, for the operator
}

// HealthChecker checks the node's running instances and records each
// one's health on its row.
type HealthChecker func(ctx context.Context) ([]Health, error)

// DefaultHealthEvery is how often the worker checks health, besides at
// startup.
const DefaultHealthEvery = time.Minute

// checkHealth runs the health checker if it is due. A database going
// down is an error line, and otter_instance_up 0: the alert.
func (w *Worker) checkHealth(ctx context.Context, now time.Time) {
	if w.CheckHealth == nil || (!w.healthChecked.IsZero() && now.Sub(w.healthChecked) < w.healthEvery()) {
		return
	}
	w.healthChecked = now
	hs, err := w.CheckHealth(ctx)
	for _, h := range hs {
		switch {
		case h.Changed && !h.Up:
			w.log().Error("instance degraded", "instance", h.InstanceID, "cluster", h.Cluster, "database", h.Database, "detail", h.Detail)
		case h.Changed:
			w.log().Info("instance ok", "instance", h.InstanceID, "cluster", h.Cluster, "database", h.Database)
		}
	}
	if err != nil {
		w.log().Error("health check", "err", err)
		return
	}
	// Only a full pass says which instances are no longer checked.
	w.Metrics.health(hs)
}

func (w *Worker) healthEvery() time.Duration {
	if w.HealthEvery > 0 {
		return w.HealthEvery
	}
	return DefaultHealthEvery
}
