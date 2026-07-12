package main

import (
	"context"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/testhooks"
)

// Clock is the injected time source (01 §6 / 03 §4: "Injected Clock … no test
// ever reads wall time"). Time is load-bearing for this slice's admission
// windows (30 accepts / 10 min) and freshness measurement, so it is injected,
// never read from the wall in a test:
//
//   - ADMISSION (admission.go): the kitchen-capacity token bucket is a sliding
//     window of width W. The 50× flash-sale test freezes the clock, fires 50×
//     the capacity of accepts inside one frozen window, and asserts exactly
//     `capacity` are admitted (± 5%) — it never sleeps.
//
// Mirrors services/order/clock.go and services/cart/clock.go.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock (wall time, UTC).
type SystemClock struct{}

// Now returns the current wall time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock is a test Clock frozen at a start time and advanced explicitly. It
// is safe for concurrent use (admission + projection goroutines read it under
// -race).
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

// nowFor resolves the effective clock for a request: an X-Test-Clock override
// (honoured ONLY in non-prod builds via libs/testhooks — the same backdoor the
// other slices use) takes precedence, else the base clock. In a prod build
// ClockFromContext always misses, so this is exactly base.Now().
func nowFor(ctx context.Context, base Clock) time.Time {
	if t, ok := testhooks.ClockFromContext(ctx); ok {
		return t.UTC()
	}
	return base.Now()
}
