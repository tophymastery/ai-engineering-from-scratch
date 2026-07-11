# libs/flags

Env-backed feature flags with a **per-request override honoured only in
non-prod** (D29). Every slice ships flag-gated (TASKS.md) so merge order is
irrelevant.

## Baseline: environment

```go
fs := flags.FromEnv()                 // reads FLAG_* (FLAG_SAGA_V1=true → "saga_v1")
if fs.Bool("saga_v1", false) { … }
mode := fs.String("risk_mode", "shadow")
```

## Per-request override (non-prod only)

A request may carry `X-Flag-Override: saga_v1=true,pricing_v1=false` to force
values for a single request (deterministic tests, preview envs).

```go
if fs.BoolCtx(r.Context(), "saga_v1", false) { … } // override wins, else env
```

This is honoured **only in non-prod builds**: the override value is read
exclusively through `libs/testhooks`, whose reader is a pure passthrough
(always `""`) in a production build because `hooks_enabled.go` is compiled out.
So in prod the override path is not merely disabled — the reading code **does not
exist** in the binary (`ci/backdoor-scan.sh` proves it), and the gateway strips
the header as an independent second layer. `flags.OverrideActive()` reports
whether this build can honour overrides at all (false in prod).

`flags_test.go` asserts the correct behaviour under **both** build tags:
`go test` (prod: override refused) and `go test -tags testhooks` (non-prod:
override wins). Zero external dependencies beyond in-repo `libs/testhooks`.
