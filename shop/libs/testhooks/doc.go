// Package testhooks implements the D29 test-backdoor middleware:
// X-Test-Clock (override the service clock) and X-Flag-Override (force a
// feature-flag value) for deterministic tests and preview envs.
//
// SAFETY MODEL (D29 layer 1 of 3 — "compiled out of prod builds"):
// the real handler lives ONLY in hooks_enabled.go, guarded by the
// `testhooks` build tag. A production build (no `-tags testhooks`) compiles
// hooks_disabled.go instead, whose Middleware is a pure passthrough and which
// contains NONE of the backdoor symbols or marker strings. ci/backdoor-scan.sh
// proves this by building with prod tags and grepping the binaries/symbols for
// the markers defined in hooks_enabled.go — any hit fails CI.
//
// The other two D29 layers live in the gateway (unconditional strip +
// prod-log alert) and are independent of this build tag.
//
// Middleware(http.Handler) http.Handler is declared in whichever of
// hooks_enabled.go / hooks_disabled.go the build tag selects.
package testhooks
