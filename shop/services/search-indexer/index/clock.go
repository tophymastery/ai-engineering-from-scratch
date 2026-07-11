package index

import (
	"sync"
	"time"
)

// Clock is the injected time source (doc 01 §6 / doc 03 §1: "Injected Clock … no
// test ever reads wall time"). Production passes time.Now; tests pass a
// ManualClock they advance explicitly, so the 5-minute rating-debounce window and
// freshness measurements are deterministic — the debounce test ADVANCES time, it
// never sleeps.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock (wall time, UTC).
type SystemClock struct{}

// Now returns the current wall time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock is a test Clock frozen at a start time and advanced explicitly. It
// is safe for concurrent use (the engine's ingest workers and query goroutines
// read it under -race).
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
