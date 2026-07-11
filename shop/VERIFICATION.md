# Verification (S-T1, S-T2)

How each Definition-of-Done item and test criterion was verified **in this
environment**, and where the environment forced an adaptation. Legend:
**full** = verified as specified · **adapted** = verified via a documented
substitute · **render-only** = manifests proven correct by rendering, not by a
live deploy. The S-T2 section is at the bottom; the S-T1 section below is
unchanged.

---

# S-T1 Verification

## Environment realities

- **Go** 1.24.7, **Node** 22, **curl** present. `go run sigs.k8s.io/kustomize/kustomize/v5`
  resolves to Kustomize **v5.8.1**.
- **Docker daemon: NOT available** (`docker info` fails). `docker-compose.yml` is
  shipped as the canonical stack definition, but `make up` detects the missing
  daemon and falls back to a **process-based boot** (`tools/dev-up.sh`): it
  compiles and runs the two std-lib Go binaries directly and health-checks them
  with curl. Observable topology is identical (gateway :8080 proxying
  `/placeholder/*` → placeholder :8081).
- **No Kubernetes cluster.** "Deploys to a cluster" cannot run here; Kustomize
  overlays are instead **verified by render** — all four overlays are built and
  every emitted YAML document is parsed (`tools/yamlcheck`).
- **CI:** no `.github/workflows` created at the repo root (this repo is not the
  shop monorepo). The pipeline lives at `ci/pipeline.yml` (GitHub-Actions-shaped,
  activates when `shop/` is extracted to its own repo) and `ci/run-local.sh` runs
  the identical stages locally.

## DoD / test-criteria matrix

| # | S-T1 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | `make up` boots an empty-but-healthy stack (gateway + placeholder) locally | **adapted** | Process-mode boot (Docker daemon absent). `make up` → both `/healthz` return `{"status":"ok",...}`; `make smoke` **3/3 pass** incl. gateway→`/placeholder/*` proxy. |
| DoD-2 | Kustomize base/overlays render for dev/preview/staging/prod; hello-world deploys to a cluster | **render-only** | `make render`: all **4 overlays** build via Kustomize v5.8.1, each emits **4 docs** (2 Deployments + 2 Services), 100% parsed by `tools/yamlcheck`. Live cluster deploy N/A (no cluster). |
| DoD-3 | Change-detection builds only affected paths (verified on a fixture) | **full** | `tools/changed-paths_test.sh` **3/3 pass**: service-only→that service; libs→ALL buildable; docs-only→nothing. |
| Test | Fresh-clone `make up` to healthy in < 10 min | **full (adapted boot)** | Warm boot measured **~0.9 s**; cold-cache build of both binaries **~10.5 s**. Fresh-clone `make up` ≈ tens of seconds ≪ 10 min. |
| Test | CI scaffold green on fixture | **adapted** | `ci/run-local.sh` runs lint+build+unit → change-detection → render → up/smoke/down, all green. `ci/pipeline.yml` mirrors these stages for the extracted repo. |
| Test | Unaffected paths skipped 100% | **full** | docs-only fixture case yields empty output (zero buildable paths); asserted in the fixture test. |

## Commands to reproduce

```
make up      # boot (docker-compose if daemon present, else process fallback)
make smoke   # 3/3 end-to-end checks against the booted stack
make render  # render + YAML-validate all 4 overlays
make test    # go vet + build + change-detection fixture (3/3)
make down    # tear down (verified: both ports closed, processes reaped)
./ci/run-local.sh   # full CI scaffold locally
```

## Deviations summary

1. **Docker unavailable** → `make up` uses a process-based fallback; compose file
   remains the canonical definition. (DoD-1: adapted.)
2. **No K8s cluster** → overlays verified by render + YAML-parse, not live deploy.
   (DoD-2: render-only.)
3. **CI location** → `ci/pipeline.yml` + `ci/run-local.sh` instead of root
   `.github/workflows`, per task instruction.

---

# S-T2 Verification (D29: test-infra safety & preview economics)

Extends the S-T1 scaffold with the full PR pipeline, the shared multi-tenant
preview, and the three-layer test-backdoor safety model. Same environment
realities as S-T1 (no Docker daemon, no K8s cluster, no live GitHub Actions);
additionally **`govulncheck` reaches the network but its vuln DB (vuln.go.dev)
is blocked by the egress proxy (HTTP 403)**, so the security gate runs a
documented offline dependency lint.

## What "reference PR green / merge blocked" means here

`ci/run-local.sh` runs the identical 10 stages as `ci/pipeline.yml`
(lint → unit → contract → build/sign → backdoor-scan → integration →
preview-e2e → security-scan → render → smoke). It `set -e`s, so **any red gate
exits nonzero** — that non-zero exit *is* the merge block. Full run:
**all 10 stages green, exit 0, ~16 s wall.**

## DoD / test-criteria matrix

| # | S-T2 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | Pipeline green end-to-end on a reference PR; merge blocked on any red gate | **adapted** | `ci/run-local.sh` runs all 10 stages (04 §1.2 order) → **exit 0**; each stage `set -e`-gated so a red gate blocks merge. `ci/pipeline.yml` mirrors the stages as jobs for the extracted repo. |
| DoD-2 | Shared preview live; per-PR URL posted; no full-stack-per-PR | **adapted** | `tools/preview.sh --pr 777`: boots ONE shared baseline, deploys only the 1 changed service, routes via `X-Preview-Tenant: pr-777`, prints URL `https://pr-777.preview.shop.io`. Manifests `deploy/preview-shared/*` + `deploy/gitops/preview-applicationset.yaml` render-verified. No per-PR full stack. |
| DoD-3 | Backdoor symbol scan in CI; gateway strip rule + prod-log alert deployed | **full** | `ci/backdoor-scan.sh` (prod build ⇒ 0 markers PASS; `--fixture` ⇒ 4 marker hits FAIL). Gateway `stripBackdoors` + WARN alert `TESTHOOK_HEADER_STRIPPED` exercised by `tools/gateway-strip_test.sh`. |
| Test | Preview cost/PR ≤ 20% of full-stack estimate | **full (modeled)** | 1 changed pod / 30-pod full catalog (TASKS.md Phase V) = **3.3% ≤ 20%**. `tools/preview.sh` computes it and exits nonzero if over budget. |
| Test | Cross-PR isolation: two PRs mutating same entity type ⇒ zero data bleed | **full** | `tools/preview-isolation_test.sh`: pr-101 `order=alpha`, pr-102 `order=beta` on the SAME shared baseline ⇒ each reads only its own write; uninvolved pr-999 reads empty. **Zero bleed, 4/4 asserts.** |
| Test | Prod-tagged fixture image with a backdoor handler ⇒ CI red | **full** | `ci/backdoor-scan.sh --fixture` builds WITH `-tags testhooks` ⇒ string marker + `applyBackdoorHooks` nm symbol found in both binaries ⇒ **exit 1**; wired as an expected-fail assertion in `make backdoor-scan`. |
| Test | Header sent to prod-mode env ⇒ stripped + alert < 1 min | **full** | `tools/gateway-strip_test.sh`: `X-Test-Clock` + `X-Flag-Override` through a `GATEWAY_MODE=prod` gateway ⇒ upstream `/headers` echoes both empty; WARN alert emitted in **0.012 s** (≪ 60 s). Control: dev-mode gateway passes the header through. |
| Test | Merge blocked on any red gate | **full** | Injecting any failing gate (e.g. reverting the strip) flips `ci/run-local.sh` to nonzero; the red-path fixture proves a real gate can go red. |

## Three-layer backdoor safety (D29) — independently verified

1. **Compiled out (build tag).** `libs/testhooks` splits `hooks_enabled.go`
   (`//go:build testhooks`) from `hooks_disabled.go` (`//go:build !testhooks`).
   Prod build (default) ⇒ marker string `SHOP_TESTHOOK_BACKDOOR_MARKER_v1` and
   symbol `applyBackdoorHooks` **absent** (`nm`/`grep` = 0). testhooks build ⇒
   both present. The header *names* are deliberately **not** scan markers (the
   gateway strip path references them legitimately).
2. **Stripped at gateway.** `stripBackdoors(mode)` deletes both headers on every
   inbound request when `GATEWAY_MODE=prod`, before proxying upstream. Proven
   even on a **prod build** (backdoors compiled out) — the strip is independent
   of the build tag.
3. **Alarmed in prod logs.** On strip, the gateway emits a 04 §3 WARN envelope
   with `error.code = TESTHOOK_HEADER_STRIPPED` to stdout immediately — the
   alert source (also an Argo Rollouts analysis metric with `failureLimit: 0`).

## Measured numbers

| Metric | Value |
|---|---|
| Full local pipeline (10 stages) | exit 0, ~16 s |
| Backdoor scan — prod build | 0 markers (PASS) |
| Backdoor scan — fixture build | 4 marker hits across 2 binaries (FAIL, expected) |
| Gateway strip → alert latency | 0.012 s (budget < 60 s) |
| Cross-PR bleed (2 tenants, same entity) | 0 |
| Preview cost ratio (1-service PR) | 1/30 = 3.3% (budget ≤ 20%) |
| Preview scale-to-zero / TTL (manifest fields) | 2 h idle / 7 d |
| Kustomize overlays render | 4/4 (unchanged from S-T1) |
| Security gate | govulncheck DB blocked → offline lint PASS (0 external deps) |

## Commands to reproduce

```
./ci/run-local.sh        # full 10-stage pipeline (the merge-gate proof)
make backdoor-scan       # D29 layer 1: prod clean + red fixture expected-fail
make strip-test          # D29 layers 2+3: strip + alert in prod mode
make preview-isolation   # 2 tenants, shared baseline, zero bleed
make preview PR=777      # shared-preview simulation + cost model
make security-scan       # govulncheck / offline dependency lint
make render-preview      # render-validate preview-shared + gitops manifests
make render              # 4/4 overlays (S-T1, still green)
make test                # unit + change-detection (S-T1 fixtures, still green)
```

## Deviations summary (S-T2)

1. **No live GitHub Actions** → `ci/pipeline.yml` carries the full stage set as
   jobs; `ci/run-local.sh` runs the identical stages and its exit code is the
   merge gate. (DoD-1: adapted.)
2. **No Docker registry / OIDC** → cosign build/sign is config-only
   (`ci/cosign.md`, rendered by the `build-sign` job), not executed. (DoD-1.)
3. **No K8s cluster** → shared preview + GitOps canary/ApplicationSet proven by
   render (`deploy/preview-shared/`, `deploy/gitops/`); `tools/preview.sh`
   simulates the per-PR changed-only + header-routing flow in process mode.
   (DoD-2: adapted / render-only.)
4. **Full-stack pod estimate** = 30 (whole TASKS.md Phase V catalog); the env
   ships only gateway + placeholder, so the ratio is modeled against the
   documented catalog. (Cost test: full-modeled.)
5. **govulncheck vuln DB blocked (403)** → documented offline dependency lint
   (external-dep surface + in-repo replace check + `go vet`). (Security: adapted.)
6. Backdoor safety (all three layers) and cross-PR isolation are **fully
   runnable** here — no adaptation.

---

# S-T3 Verification (D9: shared libs — errors, logging/otel, flags, idempotency)

Five shared libs under `libs/` (`errors`, `otel`, `logging`, `flags`,
`idempotency`), each with a README and exercised end-to-end by the reference
service `services/_placeholder` (`POST /kv`). Same environment realities as
S-T1/S-T2, plus:

- **Go module downloads work** through the proxy: `idempotency` uses `lib/pq` +
  `modernc.org/sqlite` **in tests only** (pinned to go-1.24-compatible versions:
  sqlite v1.34.5 / libc v1.61.13 / x-sys v0.28.0, so nothing forces a toolchain
  switch). The library itself and the reference service compile stdlib-only over
  `database/sql`; `errors`/`otel`/`logging`/`flags` are stdlib-only.

## DB path used: **FULL** (real ephemeral PostgreSQL)

No Docker daemon, but a **PostgreSQL 16 server binary is present**
(`/usr/lib/postgresql/16/bin`). The idempotency test harness starts an
**ephemeral PG over a unix socket** (run as the `postgres` OS user via `sudo`,
since PG refuses to run as root), migrates the D9 table, and runs the full
concurrency suite against it. The SAME suite also runs against **SQLite**
(`modernc`, pure-Go) and the transactional **MemStore** — proving the semantics
are the store's, not one engine's. If PG can't start (no binary / no sudo), the
harness logs a skip and runs on SQLite + MemStore (the documented fallback); set
`IDEMPOTENCY_SKIP_PG=1` to force it. The production DDL is PG-specific
(`libs/idempotency/migrations/0001_idempotency.pg.sql`); the store is
engine-agnostic over `database/sql` via a `Dialect`.

## DoD / test-criteria matrix

| # | S-T3 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | All five libs merged with docs + a reference service exercising each | **full** | 5 libs + READMEs; `services/_placeholder` wraps `POST /kv` in otel+logging+errors+flags+idempotency. Live run: fresh→201, replay→201+`Idempotency-Replayed: true`, diff-body→409, missing-key→400, error envelope carries the otel `trace_id`. |
| DoD-2a | Log-schema test validates the envelope | **full** | `contracts/log-schema.json` (draft-07) + `logging` emits real lines through the ingress middleware; all validate. Negative test: bad `level`/missing fields are rejected (validator can fail). |
| DoD-2b | Flag override works per-request in non-prod | **full** | Non-prod build (`-tags testhooks`): `FLAG_KV_V1=false`→403; `X-Flag-Override: kv_v1=true`→201. Prod build: same header→**still 403** (refused); `/healthz` reports `flag_override` true/false. `flags_test.go` asserts both build tags. |
| DoD-3 | Idempotency migration helper shipped | **full** | `idempotency.Migrate(ctx,db,dialect)` + `Schema()` + `migrations/0001_idempotency.pg.sql`; applied in every SQL test. |
| Test | 100 concurrent same-key ⇒ exactly 1 effect + 99 replays | **full** | `TestStormExactlyOnce` on **postgres + sqlite + mem**: 1 effect (cross-checked against the durable `effects` table) + 99 replays, 0 errors. |
| Test | Cache killed mid-storm ⇒ still exactly 1 effect | **full** | `TestStormCacheKilledMidway`: `SwappableCache.Drop()` at the 50th request (Redis-failover sim) ⇒ 1 effect + 99 replays on all 3 backends. Correctness comes from the UNIQUE constraint, not the cache. |
| Test | Same key + different body ⇒ 409 on 100% | **full** | `TestSameKeyDifferentBody409`: 100/100 ⇒ `409 IDEMPOTENCY_KEY_REUSED` on all 3 backends; effect count stays 1. |
| Test | Cold-cache p99 penalty < +20 ms | **full** | `TestColdCacheReplayP99Penalty` (300 replays warm vs cold). Measured below — all ≪ 20 ms. |

## Measured numbers

| Metric | postgres (ephemeral) | sqlite (modernc) | mem |
|---|---|---|---|
| Storm: effects / replays | 1 / 99 | 1 / 99 | 1 / 99 |
| Cache-killed storm: effects / replays | 1 / 99 | 1 / 99 | 1 / 99 |
| Different body ⇒ 409 rate | 100/100 | 100/100 | 100/100 |
| Replay p99 — warm (cache hit) | ~0.000 ms | ~0.000 ms | ~0.000 ms |
| Replay p99 — cold (DB re-read) | **1.154 ms** | 0.117 ms | 0.012 ms |
| **Cold-cache p99 penalty** (budget < +20 ms) | **+1.154 ms** ✓ | +0.117 ms ✓ | +0.012 ms ✓ |
| Full `go test` (all 5 libs, incl. PG bring-up) | — | — | ~4.2 s |

## Pipeline integration (no regression)

- `make build` now compiles all 5 libs (prod tags); `make test` runs `go vet` +
  `make test-libs` (all lib unit tests, both build tags for `flags`) +
  change-detection. **`./ci/run-local.sh` → all 10 stages green, exit 0.**
- `services/_placeholder`'s `go.mod` stays **stdlib-only + in-repo requires**
  (drivers are idempotency test-only deps, pruned from the service build), so the
  `ci/security-scan.sh` offline lint (which scans the shipped binaries incl.
  placeholder) still passes with zero external surface.
- Prod backdoor scan still clean: placeholder now imports `flags`→`testhooks`,
  but the marker/symbol appear only under `-tags testhooks`. `make backdoor-scan`
  green (prod 0 markers; `--fixture` red).

## Commands to reproduce

```
make test-libs                 # all 5 libs (idempotency spins up ephemeral PG)
make build                     # compile libs + gateway + placeholder (prod tags)
cd libs/idempotency && go test -v ./...              # full DB suite (PG+sqlite+mem)
cd libs/idempotency && IDEMPOTENCY_SKIP_PG=1 go test ./...   # sqlite+mem fallback
cd libs/flags && go test ./... && go test -tags testhooks ./...  # prod + non-prod
./ci/run-local.sh              # full 10-stage pipeline (still exit 0)
```

## Deviations summary (S-T3)

1. **DB path is FULL, not adapted** — the concurrency criteria run against a real
   ephemeral PostgreSQL (primary), with SQLite + MemStore as additional
   cross-checks. No production-semantics gap.
2. **DB drivers are test-only** — kept out of the shipped `services/_placeholder`
   binary so the security-scan's zero-external-surface invariant holds; the
   reference service exercises idempotency via the pure-Go `MemStore` (a real
   transactional store with UNIQUE-violation simulation), while the SQL durable
   path is proven by the lib's own PG/SQLite tests.
3. **`go test ./libs/...`** — libs are independent modules (per the repo's
   one-module-per-dir convention), so there is no single root module; `make
   test-libs` runs every lib's tests. All green.
4. **OTLP exporter** — no collector in this env, so `libs/otel` runs in its
   documented **no-op-exporter** mode; the W3C propagation logic (the real,
   load-bearing part) is fully tested.
