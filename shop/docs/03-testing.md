# 03 — Testing: Spawnable Systems, Generated Data, Isolated & Repeatable

Testing is a first-class subsystem: the platform must be trivial to spin up,
seed, exercise, and tear down — locally, in CI, and per pull request.

## 1. Test pyramid

| Layer | Scope | Tooling | Determinism lever |
|---|---|---|---|
| Unit | Pure logic: state machine, pricing math, dispatch scoring, idempotency middleware | Go `testing` / Jest | Injected `Clock` + seeded `Rand` (doc 01 §6) — no test ever reads wall time |
| Contract | Every BFF↔service and service↔service pair | Pact (consumer-driven); broker gates CI | Provider verification runs against pinned pacts; a service cannot merge a breaking change unnoticed |
| Integration | One service + its real PG/Kafka/Redis | Testcontainers (ephemeral containers per test package) | Fresh containers or per-run schema ⇒ no shared state |
| E2E | Whole system, real order lifecycle | Spawned stack (§2) + API-driven scenario runner | Seeded scenario data (§3) + fake providers (§5) |
| Load/chaos | Peak-hour order surge; kill-a-pod during saga | k6 profiles; Litmus experiments | Runs against preview/staging envs with golden datasets |

Merge gate: unit + contract + integration green, plus E2E on the PR's preview
environment (doc 04 §1). Nothing is "done" without an assertion at the
appropriate layer.

## 2. Spawning the system

Three ways to get a full running platform, all from one source of truth
(`deploy/` manifests + one compose file generated from them):

| Mode | Command | Use |
|---|---|---|
| Local full stack | `make up` (docker compose: all services, BFFs, PG, Kafka, Redis, otel-collector, fake providers) | day-to-day dev; `make up SERVICES=order,payment` for a slice |
| Per-PR preview env | automatic: Argo CD **ApplicationSet** creates namespace `pr-<num>` on PR open, deploys the PR's images, destroys on close | E2E gate + manual QA; URL posted on the PR |
| Scenario-in-CI | `make test-e2e` boots compose inside the CI runner | hermetic pipeline runs |

Conventions that make spawning painless:
- Every service starts healthy with **zero config** beyond env vars and its
  migrations (auto-applied on boot in non-prod).
- `make seed SCENARIO=lunch-rush` populates any running stack (§3).
- One `make smoke` runs a checkout→delivery happy path against whatever
  `BASE_URL` points at — the same script validates local, preview, and staging.

## 3. Test-data generation

- **Factory library** (`libs/factories`, Go + TS mirrors): typed builders with
  sensible defaults and overrides — `factory.Order(WithStatus(PAID),
  WithRegion("bkk"))`. Every entity has exactly one factory; tests never
  hand-roll JSON.
- **`seedctl` CLI**: reads a declarative YAML scenario and creates real data
  through the public APIs (never direct DB inserts — seeds exercise the same
  code paths as production):

```yaml
# scenarios/lunch-rush.yaml
seed: 42                # RNG seed -> identical data every run
region: bkk
merchants: {count: 25, menus_each: 30}
customers: {count: 200}
drivers:   {count: 60, online_ratio: 0.8}
orders:
  - {count: 40, state: DELIVERED}
  - {count: 10, state: DISPATCHED}
  - {count: 5,  state: PAYMENT_PENDING}
```

- **Golden datasets**: versioned scenarios (`demo-small`, `lunch-rush`,
  `load-10k`) checked into the repo; demos, E2E, and load tests share them.

## 4. Isolation & repeatability

Every test execution gets a **`run_id`** (ULID) that scopes everything it touches:

| Resource | Isolation mechanism |
|---|---|
| DB rows | All seeded aggregates carry `run_id`; integration tests use a per-run PG schema (`test_<run_id>`) — teardown = drop schema |
| Kafka | Consumer groups + (in shared envs) topic prefixes suffixed with `run_id`; no cross-run event bleed |
| Redis | Key prefix `t:<run_id>:` |
| Preview envs | Whole-namespace isolation (`pr-<num>`) — strongest boundary |
| External side effects | Impossible by construction: fake providers only outside prod (§5) |

Repeatability contract: `seed + scenario + code version ⇒ identical outcome`.
The frozen `Clock` starts at the scenario's `t0`; the seeded `Rand` drives every
probabilistic choice (dispatch jitter, sampling). Reruns are byte-identical, so
flakes are real bugs by definition. Parallel runs never share scope, so the
suite parallelizes freely.

## 5. Environment control

- **12-factor config**: env vars only; identical images promoted across envs
  (dev → preview → staging → prod); config lives in per-env overlays
  (`deploy/overlays/<env>/`), never in code.
- **Fake providers**, selected per env var, implementing the exact adapter
  interfaces:
  - `payment-sim`: scriptable PSP — card `4000…0002` always declines, `…0044`
    times out, everything else authorizes; webhooks fire like the real PSP.
  - `map-sim`: deterministic routing/ETA (straight-line × factor) so dispatch
    and tracking tests never call a paid API.
  - `notify-sink`: captures push/SMS/email into a queryable inbox for
    assertions.
- **Feature flags** (`libs/flags`, backed by config service): every risky
  feature ships dark; tests can force flags per request via a header in
  non-prod (`X-Flag-Override`), enabling both-sides testing of any flag.
- **Time control** in non-prod: `X-Test-Clock` header (accepted only when
  `ENV != prod`) advances the injected clock for a request/scenario — this is
  how saga timeouts (merchant-accept, dispatch) are tested in seconds instead of
  minutes.
