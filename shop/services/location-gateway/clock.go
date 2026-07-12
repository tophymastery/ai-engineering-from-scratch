package main

import (
	"context"
	"time"

	"github.com/shop-platform/shop/libs/testhooks"
	plane "github.com/shop-platform/shop/services/location-gateway/plane"
)

// clock.go — the injected clock at the HTTP boundary. The domain core
// (services/location-gateway/plane) defines the Clock interface + System/Manual
// clocks; main re-exports them and adds nowFor (the testhooks bridge), exactly as
// the other slices do (services/dispatch/clock.go). Time is load-bearing: the 30 s
// geo TTL, the 100 ms ingest batch window, and the 100k reconnect-storm recovery
// window are all driven off this seam so tests ADVANCE the clock, never sleep.

// Clock is the domain clock interface.
type Clock = plane.Clock

// SystemClock is the production wall clock.
type SystemClock = plane.SystemClock

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
