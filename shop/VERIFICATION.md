# Verification (S-T1–S-T6)

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

---

# S-T4 Verification (D6: `libs/sharding` + shard-hint ULIDs)

All work is under `libs/sharding/` and is **standard-library only** (no external
runtime deps), so a service adopting the router adds zero attack surface. The
tests are **pure in-memory** (no I/O per key) and use **deterministic keys**, so
every number below is exactly reproducible run-to-run — the statistical tests do
not flake.

## DoD / test-criteria matrix

| # | S-T4 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | Routing library: 256 logical → N physical, config-driven, hot-reloadable | **full** | `Router` loads JSON/YAML, `RouteKey`/`RouteID`/`Physical`; `Reload()` + mtime `Watch()` pick up a 4→8 split live (`TestRouterHotReload`); broken edit rejected, live routing untouched (`TestReloadIgnoresBrokenEdit`). |
| DoD-2 | Shard-hint ULID codec (2 hex after prefix), decode recovers shard | **full** | `NewID`/`Decode`, format `<prefix>_<HH><26-char Crockford ULID>`; full-range round-trip (`TestNewIDForShardRange`), monotonic within a ms (`TestULIDMonotonic`), valid ULID body asserted. |
| DoD-3 | Online remap tool: copy → dual-write → verify → cutover on the sandbox | **full** | `Cluster.Move` + `cmd/remapctl`; phase-hooked; `remapctl -load` run: seeded 1500, 2.6M writes/s of load, **0 write errors / 0 misroutes**, exit 0. |
| DoD-4 | Sandbox reference integration: keys across 4 fake targets, routed E2E | **full** | `Cluster` + in-memory `Store`s; `TestSandboxRoutesEndToEnd` (5000 keys land on the router-chosen target, read back), `ExampleCluster` (key→shard→hint→store→read). |
| DoD | Library + remap tool merged with docs (README) | **full** | `libs/sharding/README.md` (format, hash contract, remap sequence, results table); `libs/README.md` + Makefile updated. |
| DoD | Remap moves a logical shard under sandbox write load (concurrent writers) | **full** | `TestRemapUnderWriteLoad`: 8 writers, 2819 moves back-and-forth, 13746 dual-writes, 738k writes — **race-clean** (`go test -race`). |
| Test | 1M-key distribution within 1% of uniform (chi-square) | **full** | `TestDistribution1M`: **χ²=202.81** vs threshold **330.52** = χ²₀.₉₉₉,₂₅₅ (mean 255) ⇒ uniform not rejected at 99.9%. **~50 ms**. |
| Test | max/min shard deviation < 1% of expected | **adapted (sample size)** | `TestShardDeviationUnderOnePercent`: **0.66%** at 32M keys, **~1.6 s**. At 1M the worst shard is ~4.1% out — a hard multinomial-variance floor (σ≈1.6%/shard) for *any* uniform hash, so the 1M gate is chi-square and the literal <1% bound is met at the N where 1/√N shrinks it under 1%. |
| Test | shard-hint decode agrees with hash routing on 100% of 1M IDs | **full** | `TestDecodeAgrees1M`: **100.0000%** agreement, 0 mismatches, 0 bad bodies, **256/256** shards covered. **~0.27 s**. |
| Test | Sandbox remap under write load: zero misroutes, zero write errors | **full** | `TestRemapUnderWriteLoad`: **misroutes=0, write_errors=0** across 2819 moves under continuous concurrent load; asserted counts. |

## Measured numbers

```
TestDistribution1M                 N=1,000,000  chi2=202.81  (dof=255, χ²₀.₉₉₉=330.52)  maxdev=4.102%  ~50ms
TestShardDeviationUnderOnePercent  N=32,000,000 chi2=226.37  maxdev=0.6632% (<1%)                      ~1.6s
TestDecodeAgrees1M                 N=1,000,000  agreement=100.0000%  shards_covered=256/256            ~0.27s
TestRemapUnderWriteLoad            moves=2819  dual_writes=13746  total_writes=738,624  0 misroute/0 err ~2s (race)
```

Both 1M-scale tests finish well under the 60 s budget (chi-square ~50 ms, decode
~0.27 s; the 32M deviation demonstration ~1.6 s).

## Why the remap is misroute-free (design, not luck)

`Cluster.Put`/`Get` hold a read-lock for the **entire** operation (routing
decision + store op), and the two phase transitions (`enter dual-write`,
`verify+cutover`) take the write-lock — which waits for every in-flight
read-lock holder. So no write can be half-done across a cutover, and the moving
shard's dual-writes are paired under a dedicated mutex so `old` and `new` never
diverge, making the pre-cutover `old[shard]==new[shard]` verify an exact
equality. Backfill is copy-if-absent so a concurrent dual-write is never
clobbered. This was validated by `go test -race` (clean) and by the zero-count
assertions, and an early self-review caught (and fixed) a lock-released-before-
write bug in the hot path.

## Pipeline integration (no regression)

- `libs/sharding` added to the Makefile `LIBS` (so `make build` compiles it +
  `cmd/remapctl`, prod-tag clean) and to `test-libs` (unit suite + a dedicated
  `go test -race TestRemapUnderWriteLoad`).
- **`make test-libs` green**; **`make build` ok**; **`./ci/run-local.sh` → all 10
  stages green, exit 0** (backdoor scan, strip-test, preview isolation, render,
  smoke all unaffected — sharding is stdlib-only and imported by no shipped
  service, so `ci/security-scan.sh` surface is unchanged).

## Commands to reproduce

```
cd libs/sharding && go test ./...                                   # full suite incl. 1M + 32M
cd libs/sharding && go test -race -run TestRemapUnderWriteLoad ./... # remap under load, race-clean
cd libs/sharding && go run ./cmd/remapctl -config testdata/routing.4x256.json \
    -shard 100 -to pg-3 -load -writers 8 -duration 2s -seed 2000    # online remap demo
make test-libs        # all shared libs incl. sharding
./ci/run-local.sh     # full 10-stage pipeline (exit 0)
```

## Deviations summary (S-T4)

1. **`max/min shard deviation <1%` is asserted at 32M keys, not 1M** — at 1M the
   worst shard is ~4.1% off expected, which is the multinomial-variance floor for
   *any* uniform hash (per-shard σ ≈ 1.6% of the 3906 expected). The 1M
   uniformity gate is therefore the **chi-square** statistic (the standard test,
   and exactly what TASKS.md line 109 specifies); the literal <1% per-shard bound
   is delivered at the N where `1/√N` legitimately brings it under 1%. Both tests
   ship and pass.
2. **Config YAML is a restricted dialect**, not general YAML — dependency-light
   (D6) means stdlib-only, so no `gopkg.in/yaml.v3`. JSON is canonical; the YAML
   reader covers exactly `version`/`targets`/`assignments`. JSON⇔YAML table
   equality is asserted (`TestLoadConfigJSONAndYAMLAgree`).
3. **Remap runs against the in-memory sandbox**, not real PostgreSQL — that is
   the S-T4 scope (real-service migration is V-T26/V-T27). The Store is a real
   concurrency-safe store with copy-if-absent + snapshot primitives, so the
   copy→dual-write→verify→cutover control flow and its concurrency guarantees are
   exercised for real.

---

# S-T5 Verification (D30: contracts platform — OpenAPI + schema registry + Pact broker)

Everything lives under `contracts/` + `tools/stubgen/` + two new CI stage
scripts. The registry gate (`contracts/registryctl`) and stub generator
(`tools/stubgen`) are Go, dependency-light (stdlib + the `yaml.v3` already
vendored by `tools/yamlcheck`). Wired into the pipeline as **merge gates**:
`ci/run-local.sh` grew from 10 to **12 stages** — `[2/12] contract-validate`
and `[3/12] pact-verify` — and `ci/pipeline.yml`'s placeholder `contract` job
was replaced by the real `make contract-validate` + `make pact-verify` steps.

## DoD / test-criteria matrix

| # | S-T5 requirement | Status | How verified |
|---|---|---|---|
| DoD-1 | OpenAPI per service/BFF + convention validator | **full** | `contracts/openapi/order.v1.yaml` (02 §4.1: quotes, `POST /v1/orders` w/ Idempotency-Key, get, `:cancel`, `:capture`) + `customer-bff.v1.yaml` (home + order detail). `registryctl validate` parses each and enforces `/v1/` paths, snake_case property names, 02 §2 error envelope defined **and** `$ref`'d. Green on both files. |
| DoD-2 | Event schema registry + D30 additive-only enforcement | **full** | `contracts/events/<topic>/<version>.schema.json` for `order.created`, `order.paid`, `payment.authorized`, `dispatch.assigned`, `driver.location_updated` (+ `order.paid.v2`) — all envelope-conformant (02 §4.3, checked against `event_type` const, required set, snake_case payload). `registryctl diff` rejects remove/rename/type-change/required-addition/enum-narrowing; accepts new optional fields. `<topic>.v2` presence forces a valid, unexpired `deprecation.yaml` (topic, replaced_by, deprecation_date) on the base topic. |
| DoD-3 | Pact broker gating CI | **adapted (file-based)** | No pact-broker binary exists in this env, so the broker is **file-based**: `contracts/pacts/<consumer>__<provider>.json` (Pact-v2-shaped interactions) + `registryctl pact-verify`, which **replays each interaction against the actually-running provider** and asserts status + response shape (want-keys ⊆ got, pinned scalars equal). Seed pact `customer-bff__placeholder` (GET /healthz + idempotent POST /kv) verified against the booted placeholder: 2/2 PASS. |
| DoD-4 | Stub generator produces runnable stubs from any published contract | **full** | `tools/stubgen -spec … -port …` builds a regex router from any OpenAPI file (incl. `{param}` templates and 02 §1 `:action` verbs) and serves example/schema-derived JSON. Proven live: order.v1 stub booted, `POST /v1/orders` → 201 `PAYMENT_PENDING` body, `GET /v1/orders/{id}` → 200 order body (both curls asserted in the contract-validate stage on every CI run). |
| DoD-5 | Worked `.v2` dual-publish example in `contracts/` | **full** | `order.paid.v2` (rename `payload.total`→`order_total` + required `tip` = additive-impossible) + `order.paid/deprecation.yaml` (replaced_by, 2026-12-31) + `order.paid.v2/fixtures/` Go test: ONE producer emits both topics; gen-1 consumer reads `order.paid`, gen-2 reads `order.paid.v2`; both messages validate against the **real registry schema files** and both consumers extract their fields — green. A second test proves cross-generation incompatibility (each message FAILS the other schema), i.e. the new topic was genuinely required. |
| DoD | Registry + broker wired into the S-T2 pipeline as merge gates | **full** | `make contract-validate` / `make pact-verify` → `ci/contract-validate.sh` / `ci/pact-verify.sh`; run-local stages 2–3; pipeline.yml `contract` job now runs both for real. Any violation exits nonzero ⇒ merge blocked. |
| Test | In-place topic shape-change fixture ⇒ registry CI red (asserted) | **full** | `contracts/fixtures/registry-red/order.created.inplace-shape-change.schema.json` (rename `customer_id`→`user_id`, `item_count` int→string, new required field). `registryctl diff` exits 1 naming all 3 breaks; the stage asserts the failure expected-fail style (like the S-T2 backdoor fixture) — a fixture that *passes* fails CI. |
| Test | `.v2` dual-publish fixture ⇒ both consumer generations green | **full** | `go test` in `contracts/events/order.paid.v2/fixtures`: `TestDualPublish_BothGenerationsGreen` (gen-1 total=42550 via order.paid; gen-2 order_total=42550, tip=2000 via order.paid.v2) + `TestDualPublish_ShapesAreGenuinelyIncompatible` — both PASS, run on every CI pass. |
| Test | Breaking a published pact ⇒ provider build red (asserted) | **full** | `contracts/fixtures/pact-red/customer-bff__placeholder.broken.json` adds a `GET /v1/orders/{id}` interaction the placeholder does not implement; `pact-verify` reports `$.order_id: key missing in provider response`, exits 1; the stage asserts the failure. |
| Test | Additive change ⇒ green (control for the red path) | **full** | `contracts/fixtures/registry-green/order.created.additive.schema.json` (two new optional fields) — `registryctl diff` exit 0, asserted in the stage. |

## Pipeline integration (no regression)

- `ci/run-local.sh` **FULL 12-stage pipeline exit 0** (was 10 stages; the S-T2
  `[2/10] contract placeholder` no-op became the real `[2/12]`+`[3/12]` gates).
  All S-T1..S-T4 stages unchanged and green: make test (+ shared-lib suites +
  sharding race test), build (now also compiles registryctl + stubgen),
  backdoor-scan (+ red fixture), strip-test, preview-isolation, preview,
  security-scan, render ×4 + render-preview, up/smoke/down.
- Expected-fail count across the pipeline is now **3**: backdoor fixture (S-T2),
  registry shape-change fixture, broken-pact fixture (both S-T5).
- `registryctl` and `stubgen` have their own unit suites (`diff_test.go`:
  additive-clean + 4 breaking classes + message content; `main_test.go`: path
  regex incl. `:action`, `$ref` synthesis, example precedence) — run inside the
  contract-validate stage.

## Commands to reproduce

```
make contract-validate         # OpenAPI+registry validate, diff green+red fixtures,
                               # dual-publish test, stubgen boot + 2 curls
make pact-verify               # boots placeholder, seed pact green, broken pact red
cd contracts/registryctl && go run . validate ../../contracts
cd contracts/registryctl && go run . diff \
    ../events/order.created/v1.schema.json \
    ../fixtures/registry-red/order.created.inplace-shape-change.schema.json  # exit 1
cd contracts/events/order.paid.v2/fixtures && go test -v ./...
cd tools/stubgen && go run . -spec ../../contracts/openapi/order.v1.yaml -port 9090
./ci/run-local.sh              # full 12-stage pipeline (exit 0)
```

## Deviations summary (S-T5)

1. **Pact broker is file-based, not a pact-broker service** — the pact-broker
   binaries are not available in this environment (per the task brief). The
   adaptation keeps the Pact *semantics* that matter for the gate: pacts are
   Pact-v2-shaped JSON documents published in `contracts/pacts/` (the "broker"
   is the repo path — versioned, reviewable, single source), and verification
   replays interactions against the real running provider, red on any
   unsatisfied interaction. Swapping in a hosted broker later changes the
   fetch step only, not the verification or the CI wiring.
2. **Shape matching is subset-based**: every key pinned in the pact response
   must exist in the provider response and pinned scalars must match — the
   standard Pact postel-style rule (providers may return more). Matcher rules
   (regex/type matchers) are not implemented; none of the seeded pacts need
   them.
3. **`registryctl diff` compares JSON-Schema structure**, not full draft-07
   semantics (no `$ref`/`allOf` resolution inside event schemas — topic schemas
   in this registry are deliberately self-contained, which `validate` enforces
   via envelope conformance).
4. **stubgen synthesises from `example` or schema** — `examples` (plural) and
   content types other than `application/json` are ignored; the 02 conventions
   make JSON the only BFF/service content type.

---

# S-T6 Verification (D8 + D22: event backbone — CDC outbox, partitioned inbox, DLQ + replay)

Three new libs — `libs/eventbus` (broker abstraction + in-process Kafka
stand-in), `libs/outbox` (transactional outbox + log-based CDC relay),
`libs/inbox` (exactly-once inbox + per-group SQL DLQ) — plus the reference
sandbox service + criteria tests in `libs/eventbus/example`, the
`tools/dlqctl` park/inspect/replay CLI, and deploy templates
(`deploy/cdc/debezium-connector.json`, `deploy/alerts/event-backbone.yaml`).
All Go, dependency-light (stdlib; `modernc.org/sqlite` as a **test-only** engine,
already vendored by `libs/idempotency`). The event core is pure-stdlib so the
soak and correctness tests run under `-race`.

## Environment realities

- **No Kafka.** `MemBroker` is the in-process stand-in for the per-cell Kafka
  cluster (D5): fixed partitions, append-only per-partition logs, per-group
  cursors, ordered-per-key, at-least-once, retry-then-park. The `Broker`
  interface is Kafka-shaped so a `KafkaBroker` drops in unchanged.
- **No Debezium / PG WAL.** The relay is the `CDCTailRelay`: it tails the
  append-only outbox by monotonic `id` with a durable cursor — the sqlite/mem
  equivalent of a WAL position. `deploy/cdc/debezium-connector.json` is the
  production template (PG WAL → outbox EventRouter SMT → Kafka). **No poller**:
  the tail is an indexed `WHERE id > $cursor` range scan on an append-only
  table with a tiny cursor row — never the banned `published=false` full scan +
  per-row UPDATE (which causes the vacuum storms D8 forbids).
- **PG native partitioning is in the migrations** (`0001_outbox.pg.sql`,
  `0001_inbox.pg.sql`: `PARTITION BY RANGE (part_day)` + `DROP TABLE` cleanup).
  SQLite has no native partitioning, so tests model a partition as a `part_day`
  column and "drop partition" as a guarded `DELETE`-by-day. **render-only** for
  the DDL; the loss-free semantics are tested for real.
- **2-hour soak is not feasible here** — `go test` runs a **default 8 s** soak
  (env `SOAK_SECONDS`); a **60 s** run was executed for the recorded numbers
  below. Both sustain ≥10k events/s and hold lag p99 < 2 s throughout. The
  duration is the only thing scaled down; rate, lag SLO, partition-drop and the
  exactly-once audit are asserted for real.

## DoD / test-criteria matrix

| # | S-T6 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | Outbox/inbox/DLQ libs merged | **full** | `libs/outbox` (`WriteInTx` in caller's tx, `CDCTailRelay`, partition-drop), `libs/inbox` (`Process` exactly-once, `SQLDLQ`), `libs/eventbus` (broker + `DLQSink`). `go test -race ./...` green in all three (wired into `make test-libs`). |
| DoD-1 | Debezium connector template in `deploy/` | **full (lint-verified)** | `deploy/cdc/debezium-connector.json` — PG connector + `EventRouter` SMT routing outbox rows to the topic in the `topic` column, keyed by `agg_key` (D5), `exactly.once.support=required`. Parses via `yamlcheck` in `make render-events`. |
| DoD-2 | Replay CLI in `tools/` with runbook | **full** | `tools/dlqctl` (`list`/`inspect`/`replay`/`depth`/`seed`) + `RUNBOOK.md`. `make dlqctl-demo` runs seed→list→inspect→replay live; `go test` in the module asserts the durable handoff (parked→replayed + re-emitted into outbox). |
| DoD-2 | relay-lag + DLQ-depth alerts templated | **full (lint-verified)** | `deploy/alerts/event-backbone.yaml` PrometheusRule: relay-lag p99 (warn 1.5 s / crit 2 s), relay-stalled, DLQ-depth (warn >0 / crit >100), DLQ-park-rate. Parses via `yamlcheck` in `make render-events`. |
| DoD-3 | Reference svc publishes/consumes through full path | **full** | `libs/eventbus/example` (`go run .`): 200 orders written **business row + outbox row in one tx** → CDC relay → bus → inbox exactly-once projection. Audit orders=200 published=200 consumed=200 projection=200, lag p99 ~19 ms. |
| Test | Soak ≥10k events/s, relay lag p99 < 2 s, partition drop mid-soak with zero loss (offset/count audit) | **full (duration adapted)** | `TestSoak`. **60 s run: 1,200,000 events, sustained 20,000/s, lag p99 386 ms, p999 544 ms, max 579 ms (all < 2 s); 1,197,000 partition drops DURING the soak; published==consumed==produced==1,200,000 exactly-once, outbox stayed flat at 3,000 rows.** 8 s CI run: 160,000 events, 19,997/s, p99 1.25 ms, 156,040 drops. The drop guard refuses to drop anything past the relay cursor ⇒ zero event loss. |
| Test | 10× duplicate-delivery burst ⇒ zero duplicate side effects | **full** | `TestDuplicateDeliveryBurst`: 300 events redelivered onto the bus 10× extra (3,300 deliveries) through the **SQL inbox** ⇒ 300 unique effects, projection rows=300, applied=300. Plus `TestExactlyOnceEffect`/`TestConcurrentDuplicateBurst` in `libs/inbox` (10 concurrent same-event ⇒ exactly 1 effect). |
| Test | Poison parks without blocking (lag recovers < 60 s), replay converges exactly-once | **full** | `TestPoisonParkAndReplay` (1 partition = strict head-of-line): poison parks after **3** attempts; **200 following events keep flowing, recovery 63 ms (< 60 s)**; DLQ depth=1; then handler "fixed" + `dlq.Replay` (re-emit via outbox) ⇒ projection=201 exactly-once, DLQ depth=0; re-replay is a no-op. |
| Skip-inbox rule (D8) | naturally-idempotent handlers opt out with a code marker | **full** | `ProcessIdempotent` + `NaturallyIdempotent` marker; `TestSkipInboxRule`: 3 deliveries → 3 handler calls, **0 inbox rows**. |
| Inbox 7-day retention | partition-drop cleanup | **full** | `DropInboxOlderThanRetention` (`InboxRetention = 7d`); `TestInboxRetentionDrop`: a 10-day-old row dropped, fresh row kept. |

## Measured numbers

| Metric | 60 s soak | 8 s CI soak | Threshold |
|---|---|---|---|
| Sustained rate | **20,000 events/s** | 19,997 events/s | ≥ 10,000/s |
| Total events | 1,200,000 | 160,000 | — |
| Relay lag p99 | **386.8 ms** | 1.25 ms | < 2 s |
| Relay lag p999 / max | 544 ms / 579 ms | 3.5 ms / 8.2 ms | < 2 s |
| Partition drops during soak | **1,197,000** | 156,040 | > 0, loss-free |
| Exactly-once audit (pub==cons==prod) | 1,200,000 == all | 160,000 == all | equal |
| Poison recovery (following events flow) | — | **63 ms** | < 60 s |
| Dedupe: deliveries → effects | — | **3,300 → 300** | 0 duplicates |

## Pipeline integration (no regression)

- New libs wired into `make test-libs`: `eventbus` + `outbox` core run under
  **`-race`**, `inbox` runs, then the `example` criteria trio + the `dlqctl`
  CLI test. `make build` now also compiles `tools/dlqctl` + the example.
- `ci/run-local.sh` **FULL 12-stage pipeline exit 0**. Stage `[1/12] make test`
  now includes the S-T6 suites; stage `[10/12]` gained `make render-events`
  (Debezium connector + alert templates lint). All S-T1..S-T5 stages unchanged
  and green. `LIBS` grew to include `eventbus outbox inbox`.

## Commands to reproduce

```
cd libs/eventbus && go test -race ./...
cd libs/outbox   && go test -race ./...
cd libs/inbox    && go test ./...
cd libs/eventbus/example && go test -count=1 ./...          # soak + dedupe + poison
cd libs/eventbus/example && SOAK_SECONDS=60 go test -run TestSoak -v ./...
cd libs/eventbus/example && go run .                        # live reference svc
make dlqctl-demo                                            # park/inspect/replay CLI
make render-events                                          # connector + alerts lint
make test-libs                                             # all libs incl. S-T6
./ci/run-local.sh                                          # full 12-stage pipeline (exit 0)
```

## Deviations summary (S-T6)

1. **Kafka → `MemBroker`, Debezium → `CDCTailRelay`.** Both are in-process
   stand-ins behind the production interfaces (`Broker`, `Relay`); the Kafka
   connector + WAL wiring ship as `deploy/cdc/debezium-connector.json`. The
   append-only-log + durable-cursor shape is preserved so the swap is mechanical.
2. **PG native partitioning is render-only; SQLite models it with a `part_day`
   column.** The DDL (`PARTITION BY RANGE` + `DROP TABLE`) is in the migrations;
   the **loss-free drop semantics** (guard refuses to drop past the relay
   cursor) are tested for real, continuously, during the soak.
3. **Soak duration 8 s (CI) / 60 s (recorded), not 2 h** — infeasible in this
   sandbox. Rate (≥10k/s), lag SLO (p99 < 2 s), partition-drop-mid-soak and the
   exactly-once offset/count audit are all asserted at real scale; only wall-
   clock is shortened. "Zero autovacuum alerts" is inherent to the design (no
   UPDATE churn, partition-drop cleanup) rather than a measured PG metric here.
4. **High-rate soak uses the mem outbox + mem inbox** (`MemStore`,
   `MemProcessor`) so the backbone — not a single-writer SQLite file — is the
   thing under load. The **SQL** transactional outbox and **SQL** exactly-once
   inbox + DLQ are exercised for real by the reference service, the dedupe burst
   (SQL inbox), the poison test (SQL DLQ) and the dlqctl CLI.
5. **`dlqctl` drives a SQLite file** (`-db`) instead of a cell PG; `replay`
   re-inserts into the outbox in that DB so the running relay reprocesses it —
   the same code path production uses, minus the server.

---

# S-T7 Verification — Fake providers + factories + seedctl + golden datasets

DevEx. All checks below were **run for real in this environment** (Docker
daemon still absent → the process-mode boot from S-T1 is reused; every fake is a
std-lib binary, so process mode runs the identical topology the compose file
declares).

## Environment realities

- **Docker unavailable** → the three fakes are added to `docker-compose.yml`
  (canonical, with a per-service `-healthcheck` probe) **and** to the process-mode
  `tools/dev-up.sh`, which builds+runs the std-lib binaries and health-checks
  them on :8091/:8092/:8093. `make up` now boots gateway + placeholder + 3 fakes;
  `make down` reaps all five.
- **Go** 1.24.7; `gopkg.in/yaml.v3` resolved from the warm module cache (already
  vendored by `registryctl`/`yamlcheck`), so `seedctl` builds offline.
- **TypeScript** mirror typechecks with the environment's `tsc` 6.0.2
  (`tsc --noEmit` clean); documented to compile when BFF tooling arrives.

## DoD / test-criteria matrix

| # | S-T7 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | 3 fakes implement the **exact adapter contracts** and run in compose + E2E env | **full (adapted boot)** | `contracts/openapi/{payment-sim,map-sim,notify-sink}.v1.yaml` pass `contract-validate` (registryctl: **5 OpenAPI files OK**). Fakes boot in process-mode stack; `make smoke` **11/11** incl. behavioural checks of all three. Compose services + healthchecks added (render-only for the daemon). |
| DoD-2 | payment-sim conforms to its contract (pact/verifier or conformance test) | **full** | `TestConformsToContract`: 4 `/v1` paths present in the contract file + served with the declared success shapes, error envelope, and `text/csv` settlement. |
| DoD-3 | Every core entity has one factory; `make seed SCENARIO=lunch-rush` populates a stack via public APIs | **full** | `libs/factories`: `User/Merchant/MenuItem/Cart/Order/Driver`, one factory each (asserted). `make seed SCENARIO=lunch-rush` pushed **1145 aggregates** to the running placeholder `/kv` public API; a seeded order was read back through `GET /kv`. |
| Test | payment-sim: `…0002` declines, `…0044` times out, webhooks fire — **100% deterministic across 50 seeded reruns** | **full** | `TestDeterministic50Reruns` (`-race`): decline=**402**, timeout=**504**, webhooks ordered `authorized→captured→refunded`; **50/50 runs byte-identical**. `TestDifferentSeedDiffers` guards the RNG is seed-driven. |
| Test | Same seed + scenario ⇒ **byte-identical dataset on rerun** | **full** | `TestByteIdenticalOnRerun` (in-proc hash compare) **and** two separate `seedctl` CLI process runs: `lunch-rush` sha256 `30128634…dbbf5` on both; `demo-small` sha256 `0045176e…932d2`. |

## What was built

- **Fakes** (`services/fakes/{payment-sim,map-sim,notify-sink}`): std-lib Go,
  own modules, Dockerfiles, `-healthcheck` flag. payment-sim: seeded RNG for
  auth/capture/refund ids + latencies + webhook event ids; single FIFO webhook
  dispatcher ⇒ deterministic ordering; deterministic clock (no wall time);
  per-day settlement CSV sorted by `capture_id`.
- **`libs/factories`** (Go) + **`bffs/factories-ts`** (TS mirror, `tsc`-clean):
  typed builders, seeded RNG, deterministic shard-hint ULIDs that round-trip
  through `libs/sharding`.
- **`tools/seedctl`** (Go): YAML scenario → deterministic `Dataset` →
  canonical JSON dump + pluggable `Sink` (today `KVSink` → `/kv` public API,
  `NullSink` for dump-only).
- **Golden datasets**: `scenarios/{demo-small,lunch-rush}.yaml` (03 §3 shape).
- **Wiring**: `docker-compose.yml`, `tools/dev-up.sh`/`dev-down.sh`,
  `tools/smoke.sh`, `Makefile` (`seed` real; `build`, `test`, new `test-fakes`
  / `test-seed`), `ci/run-local.sh` stage 11 seeds `demo-small` end-to-end.

## Deviations (adapted, not skipped)

1. **`/v1` canonical paths + bare aliases.** 02 §1 forces a `/v1` major version
   and `contract-validate` enforces it, but the task spells the paths bare
   (`/psp/authorize`). Each fake serves **both**; contracts document the `/v1`
   form and the conformance test verifies against it. (services/fakes/README.md)
2. **seedctl sink = `/kv` today.** No slice service exists yet, so `KVSink`
   targets the `_placeholder` `/kv` public API; `Sink` is an interface so real
   per-entity endpoints plug in later with zero builder changes. (tools/seedctl/README.md)
3. **TS mirror determinism is intra-language.** `bffs/factories-ts` is
   byte-reproducible per seed in TS but not byte-identical to Go (the Go
   `seedctl` is the single canonical generator); the shared contract is
   shape+defaults, and the shard-hint hash mirrors `libs/sharding` exactly.
4. **Compose = render-only for the fakes** (Docker daemon absent); process-mode
   runs the identical observable topology.

## Commands to reproduce

```
make test-fakes                    # factories + 3 fakes (payment-sim -race, 50-rerun determinism, conformance)
make test-seed                     # seedctl byte-identity (in-proc + two CLI runs) + referential integrity
make contract-validate             # 3 new adapter contracts pass the gate
make up && make seed SCENARIO=lunch-rush && make smoke && make down   # end-to-end seed via public APIs
./ci/run-local.sh                  # FULL 12-stage pipeline — exits 0
```

---

# S-T8 Verification (Shared E2E environment + continuous-integration smoke)

## Environment realities

- Same as S-T1: **no Docker daemon, no K8s cluster**. The shared E2E env runs in
  **process mode** (the `tools/e2e-up.sh` fallback), identical mechanism to
  `make up`: std-lib Go binaries + curl, no infra. Everything below ran for real.
- **No V-slice service binaries exist in this repo** (they are V-T1..V-T37, not
  built here). So a slot in `mode: real` is booted from `real_cmd`, which points
  at either the genuine compiled `_placeholder` service (a real, independently
  built Go binary) or the documented contract-server alias
  (`tools/e2e-realcmd.sh`, which execs the shipped `stubgen`). This is the honest
  simulation the task authorises ("mark `real_cmd` = stub-binary alias"); in
  production `real_cmd` is the merged slice binary and the launch path
  (PORT/CONTRACT/SERVICE_NAME in env) is **byte-identical**, so the swap machinery
  under test is exactly the machinery a real merge uses.

## What was built

- **14 new minimal OpenAPI contracts** so `stubgen` can boot 100% of the topology
  (`contracts/openapi/{identity,merchant-catalog,search,cart,payment,pricing-promo,
  dispatch,location-tracking,notification,rating,settlement,merchant-bff,driver-bff,
  admin-bff}.v1.yaml`). All pass `contract-validate` (19 OpenAPI files GREEN).
- **`deploy/e2e/topology.yaml`** — the single-source manifest: 12 catalog services
  + 4 BFFs + 3 S-T7 fakes, each with `{name, port, mode, contract, real_cmd}`.
- **`tools/e2ectl`** — the one manifest+overlay resolver (plan / routes / sync /
  count / set-overlay); every `e2e-*.sh` script shells out to it.
- **`tools/e2e-up.sh` / `e2e-down.sh` / `e2e-smoke.sh` / `e2e-swap.sh` /
  `e2e-realcmd.sh`**, **`ci/post-merge-smoke.sh`**, **`ownership.yaml`**, Makefile
  targets `e2e-up/e2e-down/e2e-smoke/e2e-swap/e2e-sync/post-merge-smoke`, and a new
  E2E stage in `ci/run-local.sh`.
- **`stubgen`** gained a built-in `/healthz` (so any `/v1`-only contract is
  healthcheckable) and an opt-in `-idempotency` replay header (both backward
  compatible; existing tests untouched, one added). **`gateway`** gained
  `GATEWAY_ROUTES` file support (default placeholder route when unset).

## DoD / test-criteria matrix

| # | S-T8 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | E2E env live with 100% of the catalog present (stub or real) | **full** | `make e2e-up` boots **19 slots (12 services + 4 BFFs + 3 fakes) + gateway = 20 processes**, all `/healthz` green, in **~3 s**; gateway routing to a slot verified end-to-end. |
| DoD-2 | Stub→real swap automatic from deploy manifests | **full (adapted binary)** | Swap is driven from a runtime overlay (`.run/e2e-overlay.yaml`), never by editing the manifest; `e2e-up` re-reads manifest+overlay every invocation. `make e2e-sync` promotes any slot whose `real_cmd` binary exists (proven on a crafted manifest: `order` real_cmd → mode=real; empty slots stay stub). |
| DoD-2 | Smoke runs post-merge and pages the merging team on red | **full** | `ci/post-merge-smoke.sh <svc>` runs sync+up+smoke+down; on red emits `PAGE team="…" service="…"`. Team resolved from `ownership.yaml`. |
| Test | Smoke green at **all-stubs** | **full** | 16 stub + 3 fake + 0 real → **21/21 GREEN**. |
| Test | Smoke green at **one-real** | **full** | `e2e-swap rating` to the genuine compiled `_placeholder` binary → 15 stub + 3 fake + **1 real** → **21/21 GREEN** (rating slot serves the real service, not a stub). |
| Test | Smoke green at **all-real-but-one** | **full (documented simulation)** | Overlay flips all 16 service/BFF slots to real (rating = genuine placeholder; the other 15 = `e2e-realcmd.sh` contract-server alias) leaving `settlement` the single stub → 1 stub + 3 fake + **15 real** → **21/21 GREEN**. Proves the smoke is fully mode-agnostic across the path. |
| Test | Stub-swap latency < 15 min | **full** | `e2e-swap` measured wall-time **~1.77 s** (`SWAP_WALL_MS=1774`), gateway kept routing (no gateway restart). Budget 15 min; expectation "seconds" met. |
| Verify | Kill one service mid-smoke ⇒ smoke red | **full** | Killed the `order` slot → `e2e-smoke` **RED** (checkout hop 502, health sweep 18/19), exit 2. |
| Verify | Red-path PAGE names the owning team | **full** | Deterministically broke `pricing-promo` (healthy-but-wrong-contract binary) → `post-merge-smoke pricing-promo` emitted `PAGE team="Growth" service="pricing-promo" …` (matches `ownership.yaml`). |
| Verify | `ci/run-local.sh` FULL pipeline exit 0 with the new E2E stage | **full** | Ran end to end → **exit 0**; stage `[12/13]` booted the 20-process topology and `e2e-smoke` **21/21 GREEN**. |

## Deviations (adapted, not skipped)

1. **`real` mode = real launch path, aliased binary.** No slice service binaries
   exist in this repo, so `mode: real` boots the genuine `_placeholder` binary or
   the `e2e-realcmd.sh` contract-server. The **swap mechanism, overlay, gateway
   re-routing, and healthchecks are the production ones**; only the target binary
   is a stand-in. Documented in `tools/e2e-realcmd.sh` and `deploy/e2e/topology.yaml`.
2. **`/healthz` is a stubgen runtime endpoint, not a contract path.** Health is
   `/healthz` (unversioned) but `contract-validate` requires every path under
   `/v1/`. So each contract declares its one `/v1` resource and `stubgen` serves
   `/healthz` natively — this is what lets stubgen boot 100% of a `/v1`-only
   topology.
3. **Process mode, not Docker/K8s** (daemon/cluster absent) — identical observable
   topology; "GitOps watcher swaps stub→real on merge" is documented as the
   production form of `make e2e-sync` + `ci/post-merge-smoke.sh`.

## Commands to reproduce

```
make e2e-up                         # boot 20 processes (12 svc + 4 BFF + 3 fakes + gateway), all healthy
make e2e-smoke                      # checkout->delivery, 21/21 across the full topology (mode-agnostic)
SVC=rating REALCMD=.run/e2e/bin/placeholder-real make e2e-swap   # stub->real swap, prints SWAP_WALL_MS
make e2e-sync                       # detect merged real_cmd binaries and swap them into the overlay
ci/post-merge-smoke.sh pricing-promo  # merge-webhook target: PAGEs the owning team on red
make e2e-down --reset               # tear down + clear swaps
./ci/run-local.sh                   # FULL 13-stage pipeline incl. the E2E stage — exits 0
```

---

# V-T1 Verification (Identity & sessions slice — D4 stateless edge auth)

## What was built

- **`libs/edgeauth`** (shared, std-lib-only): ES256 JWT sign/verify (raw r||s,
  strict base64url decode), EC P-256 JWK/JWKS encode/decode with thumbprint kids,
  and the bloom-filter denylist (double-hashing, base64 snapshot + k/m/version).
  Imported by BOTH the issuer and the verifier so their crypto/bit-layout cannot
  drift.
- **`services/identity-auth`** (Go, SQLite via `database/sql` + a PG migration,
  per the S-T3 pattern): `POST /v1/auth/{register,login,refresh,revoke}`,
  `GET /v1/auth/denylist`, `GET /.well-known/jwks.json`, ops-only
  `POST /v1/auth/keys:{rotate,retire}`. PBKDF2-HMAC-SHA256 password hashing
  (Go 1.24 `crypto/pbkdf2`, std lib). 15-min ES256 access tokens (kid header) +
  opaque server-side refresh tokens (stored as a hash, rotated on refresh).
- **Gateway** (`gateway/auth.go`): local ES256 verification from a cached JWKS
  (refresh-on-unknown-kid, throttled) + a polled bloom denylist (`DENYLIST_POLL`,
  5 s); injects `X-Auth-Subject`/`X-Auth-Role`, and ALWAYS strips inbound spoofed
  copies of those headers. Flag `auth_jwt_edge`. BFF `/v1/auth/*` passthroughs
  routed to identity-auth at the gateway.
- **Contracts**: `identity.v1.yaml` extended additively (register/login/refresh/
  revoke/denylist); `customer-bff`/`driver-bff` gained `/v1/auth/*` passthroughs;
  new pact `customer-bff__identity-auth` (register + login) verified against the
  REAL service.
- **E2E**: topology `identity` slot has a real `real_cmd` (`tools/identity-realcmd.sh`);
  `make e2e-sync` swaps it in; `e2e-smoke.sh` gained an AUTH section gated on the
  identity slot being real.
- **Ops**: `deploy/alerts/auth.yaml` (revocation-lag, JWKS-fetch-failure,
  auth-error-rate; lint-verified via `make render-events`),
  `deploy/dashboards/auth-edge.json`, `docs/runbooks/key-rotation.md` +
  `tools/rotate-keys-demo.sh` rehearsal, prod-overlay flag OFF.

## DoD / test-criteria matrix

| Item | Status | Evidence |
|---|---|---|
| Demo-able end-to-end via BFF endpoints (flag on) | **full** | `e2e-smoke` AUTH §: register→login→authed→forged→refresh→revoke, 28/28 |
| Unit/contract/integration/E2E green | **full** | `make test` (edgeauth+identity-auth+gateway `-race`), `pact-verify`, `e2e-smoke`, `run-local` exit 0 |
| Key-rotation runbook rehearsed | **full** | `tools/rotate-keys-demo.sh` 13/13 + `TestKeyRotationRunbook` |
| Gateway verify adds < 1 ms p99 | **full** | `TestCriterion_P99LatencyDelta`: unauthed p99 8.9 µs, authed 290 µs, **delta 281 µs** (< 1 ms, under `-race`) |
| Forged/expired/tampered rejected 100% | **full** | `TestForgedTamperedExpired_1000` + `TestCriterion_ForgedExpiredTampered1000`: **1000/1000 = 100%** (both lib and gateway) |
| Revoked token rejected ≤ 30 s | **full** | `TestCriterion_RevocationLag`: **211 ms** at 200 ms poll; `e2e-smoke`: **5 s** at 5 s poll |
| identity-auth outage ⇒ authed error rate unchanged | **adapted** | see below |
| Dashboards + revocation-lag alert; SLO + ownership.yaml | **full/render-only** | `deploy/alerts/auth.yaml` lint-clean; `deploy/dashboards/auth-edge.json`; `ownership.yaml` identity→Identity & Trust (verified, already correct) |

## Deviations (adapted, not skipped)

1. **10-min outage → 60–90 s honest test.** `TestCriterion_IdentityOutage`
   warms the gateway JWKS+denylist cache, pre-issues 200 tokens, then **fully
   closes** the identity server and asserts **200/200 pre-issued tokens still
   verify at the edge (0 errors)** — the D4 invariant that would hold for a
   10-min (or any-length) outage, because verification makes **no hot-path call
   to identity**. A token with an unknown kid (a "new login" needing a key the
   edge can't fetch) is correctly rejected. "Only new logins/refreshes/
   revocations fail" is identity-auth's side, out of the gateway test's scope.
2. **Password KDF = PBKDF2-HMAC-SHA256** (Go 1.24 std `crypto/pbkdf2`, 210k
   iterations, per-user salt) rather than bcrypt/argon2, keeping the build
   pure-stdlib (no `x/crypto` download); the task permits an equivalent std-lib
   KDF.
3. **JWKS + key-rotation endpoints are runtime/ops paths, not in the OpenAPI
   contract** (like `/healthz`) — `contract-validate` requires every contract
   path under `/v1/`; `/.well-known/jwks.json` and `:rotate/:retire` are served
   natively and documented in the contract header + runbook.
4. **`real_cmd` builds+execs the real identity-auth binary** (`tools/identity-realcmd.sh`),
   unlike the generic stub-alias `tools/e2e-realcmd.sh`: identity is the FIRST
   real slice, so its slot boots the actual merged service.
5. **Dashboards/alerts are templates** (no live Prometheus/Grafana here) — YAML
   lint-verified; metric names (`gateway_auth_verify_seconds`,
   `gateway_denylist_age_seconds`, `gateway_jwks_*`, `gateway_auth_*`) are the
   seam a real exporter fills.

## Commands to reproduce

```
cd libs/edgeauth        && go test -race ./...          # crypto + bloom (incl. 1000-mutation)
cd services/identity-auth && go test -race ./...        # register/login/refresh/revoke/rotation
cd gateway              && go test -race ./...          # 4 criteria: p99, forged×1000, revocation, outage
make e2e-sync && make e2e-up && make e2e-smoke          # identity real; AUTH §, 28/28
tools/e2e-down.sh --reset && rm -f .run/e2e-overlay.yaml && make e2e-up && make e2e-smoke  # all-stubs, 21/21 (AUTH skipped)
tools/rotate-keys-demo.sh                               # key-rotation runbook rehearsal, 13/13
ci/pact-verify.sh                                       # customer-bff→identity-auth pact vs real service
./ci/run-local.sh                                       # FULL 13-stage pipeline — exits 0
```
