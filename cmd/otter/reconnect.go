package main

import (
	"context"
	"log/slog"
	"time"
)

// backoff spaces the worker's tries to reach the platform database while it
// is away (#199), and its log lines about them. The worker used to exit and
// let daemon(8) start it again every 10 s, logging twice each time: a long
// outage rotated everything else out of the log.
type backoff struct {
	Min, Max time.Duration // the first wait, and the most it grows to
	// LogEvery is the longest a failing stretch goes without a line, once
	// lines have thinned out to one per doubling of the tries.
	LogEvery time.Duration

	tries     int       // failed tries in this stretch
	since     time.Time // the first of them
	logged    time.Time // the last line about them
	announced bool      // a line went out since the last "reachable again"
}

// fail counts a failed try at now. It says how long to wait before the next
// one, and whether to log this one: the first, each time the tries double,
// and at least every LogEvery.
func (b *backoff) fail(now time.Time) (wait time.Duration, log bool) {
	b.tries++
	if b.tries == 1 {
		b.since = now
	}
	log = b.tries&(b.tries-1) == 0 || now.Sub(b.logged) >= b.LogEvery
	if log {
		b.logged = now
		b.announced = true
	}
	wait = b.Min
	for i := 1; i < b.tries && wait < b.Max; i++ {
		wait *= 2
	}
	return min(wait, b.Max), log
}

// reset ends a stretch: the database was up for a while.
func (b *backoff) reset() {
	b.tries, b.since, b.logged, b.announced = 0, time.Time{}, time.Time{}, false
}

// stayConnected runs run on a connection from connect until ctx ends. When
// connect fails, or run returns an error (the connection was lost), it
// waits as b says and tries again, instead of exiting. A run that lasted
// longer than b.Max ends the stretch, as the database was up in between;
// one that failed sooner keeps it, so a fault that comes back at once
// still has its waits grow and its lines thin out.
func stayConnected[C any](ctx context.Context, log *slog.Logger, b *backoff, now func() time.Time,
	connect func(context.Context) (C, error), release func(C), run func(context.Context, C) error) {
	for {
		conn, err := connect(ctx)
		if err == nil {
			if b.announced {
				log.Info("platform database reachable again", "failed_tries", b.tries, "down_for", now().Sub(b.since).Round(time.Second).String())
				b.announced = false
			}
			start := now()
			err = run(ctx, conn)
			release(conn)
			if ctx.Err() != nil {
				return
			}
			if now().Sub(start) > b.Max {
				b.reset()
			}
		}
		if ctx.Err() != nil {
			return
		}
		wait, logIt := b.fail(now())
		if logIt {
			log.Error("platform database unavailable; trying again", "err", err, "failed_tries", b.tries, "next_try_in", wait.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
