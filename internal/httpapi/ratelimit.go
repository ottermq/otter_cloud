package httpapi

import (
	"sync"
	"time"
)

// Login limits (security.md R6). Only failures count. The per-account limit
// stops guessing one person's password from many addresses; the per-IP one
// stops one address spraying many accounts. Both are fixed windows, in
// memory: a restart forgets them, which is acceptable with one api process.
const (
	accountFailures = 5
	ipFailures      = 20
	failureWindow   = 15 * time.Minute

	// maxLimiterKeys bounds memory if someone sprays random emails: past
	// it, expired windows are swept before a new key is added.
	maxLimiterKeys = 100_000
)

// limiter counts failures per key in fixed windows.
type limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	keys map[string]*failures
}

type failures struct {
	count int
	start time.Time
}

func newLimiter(max int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{max: max, window: window, now: now, keys: map[string]*failures{}}
}

// blocked reports whether key has used up its failures, and when it may
// try again.
func (l *limiter) blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.keys[key]
	if !ok {
		return 0, false
	}
	now := l.now()
	if now.Sub(f.start) >= l.window {
		delete(l.keys, key)
		return 0, false
	}
	if f.count < l.max {
		return 0, false
	}
	return f.start.Add(l.window).Sub(now), true
}

// fail records a failure for key.
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f, ok := l.keys[key]
	if ok && now.Sub(f.start) < l.window {
		f.count++
		return
	}
	if !ok && len(l.keys) >= maxLimiterKeys {
		for k, f := range l.keys {
			if now.Sub(f.start) >= l.window {
				delete(l.keys, k)
			}
		}
	}
	l.keys[key] = &failures{count: 1, start: now}
}

// reset forgets key's failures, after a success.
func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}
