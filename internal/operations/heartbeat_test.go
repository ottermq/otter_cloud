package operations_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// The acceptance of #200, the worker's half: it records its heartbeat on
// its own node's row, never another's, and reports the platform DB up and
// when its node's backups last succeeded, from the platform DB, so a
// restarted worker still shows an overdue one.
func TestWorkerHeartbeat(t *testing.T) {
	f := newFixture(t, "node-a", "node-b")
	t0 := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		node, kind, status string
		at                 time.Time
	}{
		{"node-a", "snapshot", "ok", t0},
		{"node-a", "snapshot", "ok", t0.Add(time.Hour)},
		{"node-a", "snapshot", "failed", t0.Add(2 * time.Hour)},
		{"node-b", "snapshot", "ok", t0.Add(3 * time.Hour)},
	} {
		f.exec(`INSERT INTO backup_records (cluster_id, node_id, kind, target, ref, status, created_at)
			SELECT id, node_id, $2, 'local', 'x', $3, $4 FROM clusters WHERE name = 'pg-' || $1`, r.node, r.kind, r.status, r.at)
	}

	reg := prometheus.NewRegistry()
	w := f.newWorker(nil)
	w.Poll, w.HeartbeatEvery, w.Metrics = 20*time.Millisecond, time.Hour, operations.NewMetrics(reg)
	before := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- w.Run(ctx, f.worker("node-a")) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}

	seen := map[string]*time.Time{}
	rows, err := f.db.ConnAs(dbtest.RoleMigrator).Query(context.Background(), `SELECT name, last_seen_at FROM infrastructure_nodes`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var at *time.Time
		if err := rows.Scan(&name, &at); err != nil {
			t.Fatal(err)
		}
		seen[name] = at
	}
	if a := seen["node-a"]; a == nil || a.Before(before.Add(-time.Second)) {
		t.Errorf("node-a last seen %v, want about now", a)
	}
	if b := seen["node-b"]; b != nil {
		t.Errorf("node-b last seen %v by node-a's worker, want never", b)
	}

	if up := testutil.ToFloat64(gauge(t, reg, "otter_worker_platform_db_up", nil)); up != 1 {
		t.Errorf("otter_worker_platform_db_up %v, want 1", up)
	}
	last := gauge(t, reg, "otter_backup_last_success_timestamp_seconds", map[string]string{"kind": "snapshot", "target": "local"})
	if got, want := testutil.ToFloat64(last), float64(t0.Add(time.Hour).Unix()); got != want {
		t.Errorf("last snapshot %v, want %v: node-a's newest ok one, not the failed one nor node-b's", got, want)
	}
}

// A heartbeat that fails reports the platform DB down: here the worker's
// DB is a role that is no node's worker.
func TestHeartbeatFailure(t *testing.T) {
	f := newFixture(t, "node-a")
	if _, err := operations.Heartbeat(context.Background(), f.db.PoolAs(dbtest.RoleAdmin)); err == nil {
		t.Error("heartbeat as the admin role: want an error")
	}
	reg := prometheus.NewRegistry()
	w := f.newWorker(nil)
	w.DB, w.Poll, w.Metrics = f.db.PoolAs(dbtest.RoleAdmin), 20*time.Millisecond, operations.NewMetrics(reg)
	w.Metrics.PlatformDB(true)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- w.Run(ctx, f.worker("node-a")) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-stopped
	if up := testutil.ToFloat64(gauge(t, reg, "otter_worker_platform_db_up", nil)); up != 0 {
		t.Errorf("otter_worker_platform_db_up %v, want 0", up)
	}
}

// gauge is the one series of name in reg whose labels include labels.
func gauge(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) prometheus.Collector {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue next
				}
			}
			return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name}, func() float64 { return m.GetGauge().GetValue() })
		}
	}
	t.Fatalf("no %s %v", name, labels)
	return nil
}
