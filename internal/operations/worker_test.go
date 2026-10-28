package operations_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/operations"
)

func (f *fixture) newWorker(handlers map[string]operations.Handler) *operations.Worker {
	return &operations.Worker{DB: f.db.PoolAs(f.db.NodeRole("node-a")), Name: "test", Handlers: handlers,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestWorkerRunsToAnOutcome(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		handler  operations.Handler
		status   string
		reason   string
		attempts int
	}{
		{"success", func(ctx context.Context, j *operations.Job) error {
			for _, s := range []string{"role_created", "database_created"} {
				if err := j.Checkpoint(ctx, s); err != nil {
					return err
				}
			}
			return nil
		}, "succeeded", "", 1},
		{"permanent", func(context.Context, *operations.Job) error {
			return operations.Permanent("create_failed", errors.New("flavor db-small is gone"))
		}, "failed", "create_failed", 1},
		{"transient", func(context.Context, *operations.Job) error { return errors.New("connection refused") }, "pending", "", 1},
		{"panic", func(context.Context, *operations.Job) error { panic("nil map") }, "pending", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "node-a")
			id := f.creates(1)[0]
			ran, err := f.newWorker(map[string]operations.Handler{"create": tc.handler}).RunOnce(ctx)
			if !ran || err != nil {
				t.Fatalf("RunOnce = %t, %v", ran, err)
			}
			r := f.op(id)
			if r.Status != tc.status || r.Reason != tc.reason || r.Attempts != tc.attempts || r.Locked {
				t.Errorf("operation = %+v, want %s %q", r, tc.status, tc.reason)
			}
			if tc.name == "success" && r.Step != "database_created" {
				t.Errorf("step = %q", r.Step)
			}
			if tc.name == "panic" && !strings.Contains(r.LastError, "panicked: nil map") {
				t.Errorf("last_error = %q", r.LastError)
			}
		})
	}
}

// A worker only claims the kinds it can run: one without a create handler
// leaves creates pending rather than failing them.
func TestWorkerClaimsOnlyItsKinds(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	w := f.newWorker(map[string]operations.Handler{"delete": func(context.Context, *operations.Job) error { return nil }})
	if ran, err := w.RunOnce(context.Background()); ran || err != nil {
		t.Errorf("RunOnce = %t, %v", ran, err)
	}
	if r := f.op(id); r.Status != "pending" || r.Attempts != 0 {
		t.Errorf("operation = %+v", r)
	}
}

// A handler whose lease is lost is canceled, and the worker leaves the
// operation to whoever has it now.
func TestWorkerStopsWhenTheLeaseIsLost(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	started := make(chan struct{})
	var canceled atomic.Bool
	w := f.newWorker(map[string]operations.Handler{"create": func(ctx context.Context, _ *operations.Job) error {
		close(started)
		<-ctx.Done()
		canceled.Store(true)
		return ctx.Err()
	}})
	w.Lease = 300 * time.Millisecond
	go func() {
		<-started
		f.exec(`UPDATE operations SET locked_by = 'other/000000000000' WHERE id = $1`, id)
	}()
	ran, err := w.RunOnce(context.Background())
	if !ran || err != nil || !canceled.Load() {
		t.Fatalf("RunOnce = %t, %v; canceled %t", ran, err, canceled.Load())
	}
	var by string
	if err := f.db.ConnAs("otter_migrator").QueryRow(context.Background(), `SELECT locked_by FROM operations WHERE id = $1`, id).Scan(&by); err != nil || by != "other/000000000000" {
		t.Errorf("locked_by = %q, %v", by, err)
	}
	if r := f.op(id); r.Status != "running" {
		t.Errorf("operation = %+v, want it still running for the other worker", r)
	}
}

// A long run keeps its lease: no other worker can take it over.
func TestWorkerRenewsItsLease(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	other := f.worker("node-a")
	w := f.newWorker(map[string]operations.Handler{"create": func(ctx context.Context, _ *operations.Job) error {
		time.Sleep(500 * time.Millisecond) // over two leases
		if _, ok, _ := operations.Claim(ctx, other, "w2", creates, time.Minute); ok {
			return operations.Permanent("create_failed", errors.New("taken over while renewing"))
		}
		return nil
	}})
	w.Lease = 200 * time.Millisecond
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.op(id); r.Status != "succeeded" || r.Attempts != 1 {
		t.Errorf("operation = %+v, want succeeded on the first attempt", r)
	}
}

// An operation the worker kept dying on fails without running again.
func TestWorkerGivesUpAfterRepeatedCrashes(t *testing.T) {
	f := newFixture(t, "node-a")
	id := f.creates(1)[0]
	f.exec(`UPDATE operations SET status = 'running', attempts = $2, locked_by = 'dead/000000000000',
		locked_until = now() - interval '1 second', started_at = now() WHERE id = $1`, id, operations.MaxAttempts)
	var calls atomic.Int32
	w := f.newWorker(map[string]operations.Handler{"create": func(context.Context, *operations.Job) error {
		calls.Add(1)
		return nil
	}})
	if ran, err := w.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("RunOnce = %t, %v", ran, err)
	}
	if r := f.op(id); calls.Load() != 0 || r.Status != "failed" || r.Reason != "create_failed" || !strings.Contains(r.LastError, "lease expired") {
		t.Errorf("handler ran %d times; operation = %+v", calls.Load(), r)
	}
}

// A new operation wakes a waiting worker at once, not at the next poll.
func TestWorkerWakesOnNotify(t *testing.T) {
	f := newFixture(t, "node-a")
	ran := make(chan string, 1)
	w := f.newWorker(map[string]operations.Handler{"create": func(_ context.Context, j *operations.Job) error {
		ran <- j.ID
		return nil
	}})
	w.Poll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- w.Run(ctx, f.worker("node-a")) }()
	defer func() {
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("Run = %v", err)
		}
	}()

	time.Sleep(200 * time.Millisecond) // listening, idle
	id := f.creates(1)[0]
	select {
	case got := <-ran:
		if got != id {
			t.Errorf("ran %s, want %s", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker didn't wake up for the new operation")
	}
}
