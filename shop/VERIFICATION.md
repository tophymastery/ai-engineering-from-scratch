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
