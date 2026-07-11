package rank

import (
	"sync"
	"time"
)

// Clock is the injected time source (doc 01 §6 / doc 03 §1: "Injected Clock … no
// test ever reads wall time"). Production passes SystemClock; tests pass a
// ManualClock they advance explicitly, so the auto-fallback detection window is
// deterministic — the fallback test ADVANCES time, it never sleeps. This mirrors
// the established V-T4 pattern (services/search-indexer/index/clock.go); the
// wider platform test backdoor (X-Test-Clock, libs/testhooks) feeds the same
// injected-clock seam at the HTTP boundary in non-prod builds.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock (wall time, UTC).
type SystemClock struct{}

// Now returns the current wall time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock is a test Clock frozen at a start time and advanced explicitly. It
// is safe for concurrent use (the ranker's request goroutines and the health
// monitor read it under -race).
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewManualClock builds a frozen clock at t0.
func NewManualClock(t0 time.Time) *ManualClock { return &ManualClock{t: t0.UTC()} }

// Now returns the frozen time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the frozen clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
