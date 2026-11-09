package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/config"
)

// The waits double from Min up to Max; lines go out on the first failure,
// each time the failures double, and at least every LogEvery.
func TestBackoff(t *testing.T) {
	b := &backoff{Min: time.Second, Max: time.Minute, LogEvery: 10 * time.Minute}
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	var waits []time.Duration
	var logged []int
	for i := 1; i <= 200; i++ {
		wait, log := b.fail(now)
		waits = append(waits, wait)
		if log {
			logged = append(logged, i)
		}
		now = now.Add(wait)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if waits[i] != w {
			t.Errorf("wait %d = %s, want %s", i+1, waits[i], w)
		}
	}
	// 200 tries take over three hours: 1, 2, 4, 8, 16 by doubling (the
	// first 16 take about 10 minutes), then one a LogEvery.
	if len(logged) > 25 || logged[0] != 1 || logged[1] != 2 || logged[4] != 16 {
		t.Errorf("logged on tries %v", logged)
	}
	for i := 1; i < len(logged); i++ {
		if gap := logged[i] - logged[i-1]; gap > 11 { // a try a minute: ≤ LogEvery apart
			t.Errorf("tries %d and %d logged %d tries apart", logged[i-1], logged[i], gap)
		}
	}
}

// A database that is away for a long stretch costs a few lines and no
// exit; when it is back, one line says so, and the worker runs on it.
func TestStayConnected(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A clock that moves a minute per call, so the stretch is long, while
	// the real waits stay short.
	clock := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(time.Minute); return clock }
	b := &backoff{Min: time.Microsecond, Max: 50 * time.Microsecond, LogEvery: 10 * time.Minute}

	tries, runs, released := 0, 0, 0
	connect := func(context.Context) (int, error) {
		tries++
		if tries <= 100 {
			return 0, errors.New("connection refused")
		}
		return tries, nil
	}
	run := func(ctx context.Context, conn int) error {
		runs++
		if runs == 1 {
			return errors.New("wait for operations: conn closed") // lost at once
		}
		cancel()
		return nil
	}
	stayConnected(ctx, log, b, now, connect, func(int) { released++ }, run)

	out := buf.String()
	if runs != 2 || released != 2 {
		t.Errorf("runs = %d, released = %d, want 2 and 2", runs, released)
	}
	if n := strings.Count(out, "trying again"); n == 0 || n > 25 {
		t.Errorf("%d failure lines for %d tries:\n%s", n, tries, out)
	}
	if n := strings.Count(out, "reachable again"); n != 2 {
		t.Errorf("%d recovery lines, want 2 (after the stretch, after the lost run):\n%s", n, out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("no line names the cause:\n%s", out)
	}
}

// Cancelling while it waits returns at once.
func TestStayConnectedStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := &backoff{Min: time.Hour, Max: time.Hour, LogEvery: time.Hour}
	done := make(chan struct{})
	go func() {
		stayConnected(ctx, slog.New(slog.DiscardHandler), b, time.Now,
			func(context.Context) (int, error) { return 0, errors.New("down") }, func(int) {}, func(context.Context, int) error { return nil })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stayConnected didn't return after cancel")
	}
}

// The acceptance of #199: with the platform database away, the worker
// doesn't exit; it waits, longer each time, with a line on the first
// failures only, and stops when told to.
func TestWork_WaitsForThePlatformDatabase(t *testing.T) {
	dir, env := workerFiles(t)
	env["OTTER_METRICS_ADDR"] = "127.0.0.1:0"
	withEnv(t, env)
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "pg_dump"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var errOut bytes.Buffer
	cfg, ok := loadConfig("worker", config.Needs{Database: true, JailDeck: true, Cluster: true, Metrics: true, Dumps: true, Backup: true}, &errOut)
	if !ok {
		t.Fatal(errOut.String())
	}
	var buf syncBuffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- work(ctx, cfg, log) }()
	// Tries at 0, 1 and 3 s; the fourth would be at 7 s.
	time.Sleep(4 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("work returned while the database was away: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("work = %v, want nil once cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("work didn't return after cancel")
	}
	out := buf.String()
	if n := strings.Count(out, "trying again"); n != 2 { // tries 1 and 2; 3 isn't a doubling
		t.Errorf("%d failure lines, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, `"next_try_in":"2s"`) {
		t.Errorf("no growing wait in:\n%s", out)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger in another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
