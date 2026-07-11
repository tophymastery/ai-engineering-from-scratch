//go:build !testhooks

// This file is compiled for PRODUCTION builds (the default — no `testhooks`
// tag). Middleware is a pure passthrough: no header names, no backdoor marker,
// no applyBackdoorHooks symbol. This is D29 layer 1: the backdoors do not
// exist in the shipped binary.
package testhooks

import (
	"context"
	"net/http"
	"time"
)

// Enabled reports whether the test backdoors are compiled in. False here.
const Enabled = false

// Middleware is a no-op in production builds.
func Middleware(next http.Handler) http.Handler { return next }

// ClockFromContext always reports "no override" in production builds.
func ClockFromContext(context.Context) (time.Time, bool) { return time.Time{}, false }

// FlagOverrideFromContext always reports "no override" in production builds.
func FlagOverrideFromContext(context.Context) (string, bool) { return "", false }
