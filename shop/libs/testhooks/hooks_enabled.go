//go:build testhooks

// This file is compiled ONLY when the `testhooks` build tag is set (test,
// preview, and the deliberate red-path fixture). It MUST NOT be present in a
// production binary — ci/backdoor-scan.sh greps for backdoorMarker and the
// applyBackdoorHooks symbol to enforce that.
package testhooks

import (
	"context"
	"log"
	"net/http"
	"time"
)

// Enabled reports whether the test backdoors are compiled in. True here.
const Enabled = true

// backdoorMarker is a unique, greppable string that exists in the binary ONLY
// when this file is compiled. The gateway strip logic deliberately does NOT
// reference it, so a prod binary that (correctly) still knows the header names
// for stripping will NOT contain this marker. The scan keys on this.
const backdoorMarker = "SHOP_TESTHOOK_BACKDOOR_MARKER_v1"

// Header names the backdoors read.
const (
	TestClockHeader    = "X-Test-Clock"
	FlagOverrideHeader = "X-Flag-Override"
)

type ctxKey int

const (
	clockKey ctxKey = iota
	flagKey
)

// applyBackdoorHooks mutates the request context from the backdoor headers.
// Its distinctive name is a second scan target (via `go tool nm`) independent
// of the string marker.
func applyBackdoorHooks(r *http.Request) *http.Request {
	ctx := r.Context()
	if v := r.Header.Get(TestClockHeader); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			ctx = context.WithValue(ctx, clockKey, t)
		}
	}
	if v := r.Header.Get(FlagOverrideHeader); v != "" {
		ctx = context.WithValue(ctx, flagKey, v)
	}
	return r.WithContext(ctx)
}

// Middleware applies the test backdoors, then calls next. Guarded build:
// present only under `-tags testhooks`.
func Middleware(next http.Handler) http.Handler {
	_ = backdoorMarker
	log.Printf("testhooks: middleware ACTIVE (marker=%s) — non-prod build only", backdoorMarker)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, applyBackdoorHooks(r))
	})
}

// ClockFromContext returns the overridden clock, if any.
func ClockFromContext(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(clockKey).(time.Time)
	return t, ok
}

// FlagOverrideFromContext returns the forced flag value, if any.
func FlagOverrideFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(flagKey).(string)
	return v, ok
}
