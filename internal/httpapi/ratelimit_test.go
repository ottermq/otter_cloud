package httpapi

import (
	"strconv"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	l := newLimiter(3, 10*time.Minute, func() time.Time { return now })

	for i := range 3 {
		if _, blocked := l.blocked("a"); blocked {
			t.Fatalf("blocked after %d failures, limit is 3", i)
		}
		l.fail("a")
		now = now.Add(time.Minute)
	}
	wait, blocked := l.blocked("a")
	if !blocked || wait != 7*time.Minute {
		t.Fatalf("after 3 failures: blocked=%v wait=%s, want blocked for 7m", blocked, wait)
	}
	if _, blocked := l.blocked("b"); blocked {
		t.Error("another key is blocked")
	}

	// The window ends: the limit resets.
	now = now.Add(7 * time.Minute)
	if _, blocked := l.blocked("a"); blocked {
		t.Error("still blocked after the window")
	}
	l.fail("a")
	if _, blocked := l.blocked("a"); blocked {
		t.Error("one failure in a new window blocks")
	}

	// A success forgets the failures.
	l.fail("a")
	l.fail("a")
	l.reset("a")
	if _, blocked := l.blocked("a"); blocked {
		t.Error("blocked after reset")
	}
}

func TestLimiterSweepsExpiredKeys(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	l := newLimiter(3, time.Minute, func() time.Time { return now })
	for i := range maxLimiterKeys {
		l.fail(strconv.Itoa(i))
	}
	now = now.Add(2 * time.Minute)
	l.fail("new")
	if n := len(l.keys); n != 1 {
		t.Errorf("%d keys after the sweep, want 1", n)
	}
}
