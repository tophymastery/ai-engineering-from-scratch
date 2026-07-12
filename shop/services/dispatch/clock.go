package main

import (
	"context"
	"time"

	match "github.com/shop-platform/shop/services/dispatch/match"
	"github.com/shop-platform/shop/libs/testhooks"
)

// clock.go — the injected clock at the HTTP boundary. The domain core
// (services/dispatch/dispatch) defines the Clock interface + System/Manual clocks;
// main re-exports them and adds nowFor, the testhooks bridge, exactly as the other
// slices do (services/merchant-queue/clock.go). Time is load-bearing here: the
// 10 s reservation TTL, the 1–2 s tick, and the 24 h leak soak are all driven off
// this seam so tests ADVANCE the clock and never sleep.

// Clock is the domain clock interface.
type Clock = match.Clock

// SystemClock is the production wall clock.
type SystemClock = match.SystemClock

// nowFor resolves the effective clock for a request: an X-Test-Clock override
// (honoured ONLY in non-prod builds via libs/testhooks) takes precedence, else the
// base clock. In a prod build ClockFromContext always misses, so this is exactly
// base.Now().
func nowFor(ctx context.Context, base Clock) time.Time {
	if t, ok := testhooks.ClockFromContext(ctx); ok {
		return t.UTC()
	}
	return base.Now()
}
