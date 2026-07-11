package main

import (
	"context"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/testhooks"
)

// Clock is the injected time source (doc 01 §6 / doc 03 §1: "Injected Clock … no
// test ever reads wall time"). Time is load-bearing for TWO correctness
// properties of this slice, so it is injected, never read from the wall in a
// test:
//
//   - SURGE WINDOWS: the deterministic pricing engine (pricing.go) decides
//     whether a surge fee applies from the quote's issue time. A frozen clock
//     lets the math be unit-tested byte-identically across surge/off-peak.
//   - QUOTE EXPIRY: a quote carries a signed expires_at (issue + 10 min). The
//     tamper/expiry sweep advances a frozen clock past expiry to prove an expired
//     quote is rejected with 422 — it advances time, it never sleeps.
//
// Mirrors the V-T7 cart / V-T6 feed-cache pattern (services/cart/clock.go).
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock (wall time, UTC).
type SystemClock struct{}

// Now returns the current wall time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock is a test Clock frozen at a start time and advanced explicitly. It
// is safe for concurrent use (the perf test's request goroutines read it under
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
// (honoured ONLY in non-prod builds, via libs/testhooks — the same backdoor
// V-T1..V-T7 use) takes precedence, else the base clock. So an E2E/preview
// caller can freeze the surge window / expiry deterministically without the
// service reading wall time. In a prod build ClockFromContext always misses, so
// this is exactly base.Now().
func nowFor(ctx context.Context, base Clock) time.Time {
	if t, ok := testhooks.ClockFromContext(ctx); ok {
		return t.UTC()
	}
	return base.Now()
}
