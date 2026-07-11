# TASKS — 100M-User Platform Roadmap (Setup + Parallel Fullstack Slices)

Implements decisions **D1–D30** in [docs/05-scale-100m.md](docs/05-scale-100m.md)
and the base blueprint (docs 01–04). Structure: **one ordered SETUP phase
(S-T1…S-T8)**, then **37 vertical slices (V-T1…V-T37) that all run in
parallel** — every non-setup task depends only on S-tasks (or nothing), never
on another slice.

## Parallel execution rules

- **Contracts, not code, are the integration surface.** Every slice publishes
  its OpenAPI spec, event schemas, and Pact pacts to the registry/broker
  (S-T5) *before* implementation. Other slices build against those published
  contracts — never against another slice's running code or repo internals.
- **Fakes stand in for every neighbor.** The setup fakes (payment-sim,
  map-sim, notify-sink — S-T7) plus contract-generated service stubs mean no
  slice ever waits on another slice's implementation. Saga-adjacent slices
  (order / payment / dispatch) develop against each other's published
  contracts + fakes and integrate **continuously in the shared E2E env**
  (S-T8), where whichever real implementations exist replace their stubs
  automatically.
- **Contract changes go through the registry, not through cross-team
  blocking:** additive-only within a topic/version; shape changes are a new
  topic/version with a dual-publish window and enforced deprecation date
  (D30). A slice needing a contract change PRs the contract; CI (Pact +
  registry rules) informs every affected slice — no meetings on the critical
  path.
- **Every slice ships flag-gated**, so merge order is irrelevant: any subset
  of completed slices yields a working system with the rest stubbed by fakes.

## Per-task template (normative)

```markdown
### <ID>: <Title>
- **Team:** <one team from D28>
- **Decisions implemented:** D<x>[, D<y>] | none (base blueprint)
- **Depends on:** <S-task IDs | none>
- **Scope:** 2-4 sentences; what is in and explicitly what is out.
- **Definition of Done:** bullets, each independently checkable; every slice
  includes "demo-able end-to-end via its BFF endpoint(s) against fakes in the
  shared E2E env".
- **Test criteria:** automated assertions with numeric pass thresholds; load
  criteria cite the capacity model (D24) at 1.5x.
- **Effort:** <= 2 weeks.
```

Conventions: every slice delivers the full vertical — BFF endpoint(s) →
service(s) → DB/migrations → events → tests at all four levels
(unit/contract/integration/E2E) → dashboards + alerts → deployed behind a
feature flag. Money-path chaos criteria always assert **zero duplicate
charges, zero lost orders**. Every new service's DoD includes SLO + runbook +
`ownership.yaml` entry.

---

## Phase S — SETUP (ordered; the only sequential work)

### S-T1: Monorepo scaffold + K8s/compose baseline
- **Team:** DevEx
- **Decisions implemented:** none (base blueprint, 04 §1.1)
- **Depends on:** none
- **Scope:** Monorepo layout (services/, bffs/, libs/, contracts/, deploy/, scenarios/, tools/), Kustomize base + overlays, one generated compose file, `make up/seed/smoke` skeletons, path-based change detection. Out: any business service.
- **Definition of Done:**
  - `make up` boots an empty-but-healthy stack (gateway + placeholder service) locally.
  - Kustomize base/overlays render for dev/preview/staging/prod; hello-world deploys to a cluster.
  - Change-detection builds only affected paths (verified on a fixture PR).
- **Test criteria:**
  - Fresh-clone `make up` to healthy in < 10 min on a dev machine.
  - CI scaffold job green on the fixture PR; unaffected paths skipped 100%.
- **Effort:** 1 week.

### S-T2: CI pipeline + shared multi-tenant preview envs + prod-safety gates
- **Team:** DevEx
- **Decisions implemented:** D29
- **Depends on:** S-T1
- **Scope:** Full PR pipeline (lint → unit → contract → build/sign → integration → preview E2E → security scan), GitOps delivery with canary/rollback, shared multi-tenant preview infra (per-PR deploy of changed services + header routing over a shared baseline, scale-to-zero 2 h, TTL 7 d), and prod-safety: test backdoors (`X-Test-Clock`/`X-Flag-Override`) compiled out of prod builds, stripped at gateway, alarmed in prod logs.
- **Definition of Done:**
  - Pipeline green end-to-end on a reference PR; merge blocked on any red gate.
  - Shared preview live; per-PR URL posted; no full-stack-per-PR pattern created.
  - Backdoor symbol scan in CI; gateway strip rule + prod-log alert deployed.
- **Test criteria:**
  - Preview cost/PR ≤ 20% of a full-stack estimate; cross-PR isolation: two PRs mutating the same entity type show zero data bleed.
  - Prod-tagged fixture image containing a backdoor handler ⇒ CI red; header sent to prod-mode env ⇒ stripped + alert < 1 min.
- **Effort:** 2 weeks.

### S-T3: Shared libs — errors, logging/otel, flags, idempotency (PG-durable)
- **Team:** Storage/DB
- **Decisions implemented:** D9
- **Depends on:** S-T1
- **Scope:** `libs/errors` (code registry), `libs/logging` (04 §3 envelope + per-route sampling classes), `libs/otel`, `libs/flags`, and `libs/idempotency` per D9: dedupe = `UNIQUE(idempotency_key)` insert in the caller's own PG transaction; Redis as read-through cache + IN_FLIGHT advisory only. Wire protocol of 02 §3 unchanged.
- **Definition of Done:**
  - All five libs merged with docs and a reference service exercising each.
  - Log-schema test validates the envelope; flag override works per-request in non-prod.
  - Idempotency migration helper shipped for adopting slices.
- **Test criteria:**
  - 100 concurrent same-key requests ⇒ exactly 1 effect, 99 replays; Redis killed mid-storm ⇒ still exactly 1 effect; cold-cache p99 penalty < +20 ms.
  - Same key + different body ⇒ 409 on 100% of attempts.
- **Effort:** 2 weeks.

### S-T4: `libs/sharding` + shard-hint ULIDs
- **Team:** Storage/DB
- **Decisions implemented:** D6
- **Depends on:** S-T1
- **Scope:** Routing library: 256 logical shards → physical map (config-driven, hot-reloadable), shard-hint ULID codec (2 hex chars after prefix), online remap tool; sandbox reference integration. Out: migrating real services (V-T26/V-T27).
- **Definition of Done:**
  - Library + remap tool merged with docs; sandbox service routes end-to-end.
  - Remap moves a logical shard under sandbox write load.
- **Test criteria:**
  - 1M-key distribution within 1% of uniform (chi-square); shard-hint decode agrees with hash routing on 100% of 1M IDs.
  - Sandbox remap: zero misroutes, zero write errors.
- **Effort:** 2 weeks.

### S-T5: Contracts platform — OpenAPI + schema registry + Pact broker
- **Team:** Data Platform
- **Decisions implemented:** D30
- **Depends on:** S-T1
- **Scope:** `contracts/` as the single source: OpenAPI per service/BFF, event schema registry with D30 rules (additive-only per topic; shape change ⇒ new `.v2` topic + dual-publish + enforced deprecation date), Pact broker gating CI, contract-stub generation so slices can develop against unbuilt neighbors.
- **Definition of Done:**
  - Registry + broker live and wired into the S-T2 pipeline as merge gates.
  - Stub generator produces runnable service stubs from any published contract.
  - Worked `.v2` dual-publish example in `contracts/`.
- **Test criteria:**
  - Fixture PR with an in-place topic shape change ⇒ registry CI red; `.v2` dual-publish fixture ⇒ both consumer generations green.
  - Breaking a published pact ⇒ provider build red.
- **Effort:** 2 weeks.

### S-T6: Event backbone — CDC outbox, partitioned inbox, DLQ + replay
- **Team:** Data Platform
- **Decisions implemented:** D8, D22
- **Depends on:** S-T1, S-T3, S-T5
- **Scope:** Debezium log-based CDC outbox platform (no pollers), time-partitioned outbox/inbox with partition-drop cleanup (7-day inbox) and documented skip-inbox rule, DLQ per consumer group in the shared consumer lib (park after 3 retries) + park/inspect/replay CLI.
- **Definition of Done:**
  - Outbox/inbox/DLQ libs merged; Debezium connector template in `deploy/`.
  - Replay CLI in `tools/` with runbook; relay-lag + DLQ-depth alerts templated.
  - Reference service publishes/consumes through the full path in the E2E env.
- **Test criteria:**
  - 10k events/s soak 2 h ⇒ relay lag p99 < 2 s, zero autovacuum alerts; partition drop with zero event loss (offset audit).
  - 10× duplicate-delivery burst ⇒ zero duplicate side effects; poison message parks without blocking (lag recovers < 60 s), replay converges exactly-once.
- **Effort:** 2 weeks.

### S-T7: Fake providers + factories + seedctl + golden datasets
- **Team:** DevEx
- **Decisions implemented:** none (base blueprint, 03 §3/§5)
- **Depends on:** S-T1, S-T5
- **Scope:** `payment-sim` (scriptable PSP incl. webhooks and settlement files, decline/timeout cards), `map-sim` (deterministic routing/ETA), `notify-sink` (queryable inbox); `libs/factories` (Go + TS) and `seedctl` with declarative scenarios; golden datasets `demo-small` and `lunch-rush` (skewed load datasets live in V-T31).
- **Definition of Done:**
  - All three fakes implement the exact adapter contracts from S-T5 and run in compose + E2E env.
  - Every core entity has one factory; `make seed SCENARIO=lunch-rush` populates any stack via public APIs.
- **Test criteria:**
  - payment-sim: card `…0002` declines, `…0044` times out, webhooks fire — 100% deterministic across 50 seeded reruns.
  - Same seed + scenario ⇒ byte-identical dataset on rerun.
- **Effort:** 2 weeks.

### S-T8: Shared E2E environment + continuous-integration smoke
- **Team:** DevEx
- **Decisions implemented:** none (base blueprint, 03 §2)
- **Depends on:** S-T2, S-T7
- **Scope:** The standing shared E2E env where all slices integrate continuously: full topology with every not-yet-built service backed by its contract stub/fake, auto-swapped for the real implementation on each slice's merge; `make smoke` checkout→delivery path runs on every merge.
- **Definition of Done:**
  - E2E env live with 100% of the service catalog present (stub or real).
  - Stub→real swap is automatic from deploy manifests; smoke runs post-merge and pages the merging team on red.
- **Test criteria:**
  - Smoke (checkout→delivery vs fakes) green with zero real services and with any partial mix (verified at all-stubs, one-real, and all-real-but-one configurations).
  - Stub-swap latency: real service live in E2E < 15 min after merge.
- **Effort:** 2 weeks.

---

## Phase V — PARALLEL FULLSTACK SLICES (no ordering; depend only on S-tasks)

### V-T1: Identity & sessions slice
- **Team:** Identity & Trust
- **Decisions implemented:** D4
- **Depends on:** S-T3, S-T5, S-T8
- **Scope:** Register/login/refresh via customer-bff and driver-bff; `identity-auth` service + DB; 15-min ES256 JWTs verified locally at the gateway (cached JWKS) with a ≤ 30 s replicated bloom denylist; no introspection on the hot path.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (flag `auth_jwt_edge` on).
  - Unit/contract/integration/E2E tests merged and green; key-rotation runbook rehearsed.
  - Gateway auth dashboards + revocation-lag alert live; SLO + `ownership.yaml`.
- **Test criteria:**
  - Gateway verification adds < 1 ms p99; forged/expired tokens rejected 100%.
  - Revoked token rejected ≤ 30 s later; identity-auth 10-min outage ⇒ authenticated-traffic error rate unchanged.
- **Effort:** 2 weeks.

### V-T2: Profile, residency & erasure slice
- **Team:** Identity & Trust
- **Decisions implemented:** D3
- **Depends on:** S-T3, S-T5, S-T6
- **Scope:** `identity-profile` with per-jurisdiction PII stores (in-country for ID/VN); all events carry `usr_`/`adr_` tokens only; per-user data keys with crypto-shredding erasure API; CI-validated data-inventory + retention register; profile CRUD via customer-bff.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (profile CRUD + erasure demo).
  - Four test levels green; PII scanner in CI; network policy denies non-owning-cell PII access.
  - Register checked in; erasure runbook + DPO sign-off recorded.
- **Test criteria:**
  - Scanner: zero raw PII in golden-traffic events/logs; unregistered-table fixture ⇒ CI red.
  - Erasure fixture: PII unreadable across stores + backups ≤ 72 h while order replay with tokens still succeeds.
- **Effort:** 2 weeks.

### V-T3: Merchant catalog & menus slice
- **Team:** Discovery
- **Decisions implemented:** none (base blueprint, 01 §1)
- **Depends on:** S-T3, S-T5, S-T6
- **Scope:** `merchant-catalog` service + DB; menu editor and store-status endpoints via merchant-bff (ETag/If-Match); publishes `menu.updated`, `store.status_changed` per contract.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (flag `catalog_v1` on).
  - Four test levels green incl. pacts for search/cart consumers; dashboards + alerts live.
  - Stale-write protection verified (412 on ETag mismatch).
- **Test criteria:**
  - Menu CRUD p99 < 200 ms at 1k RPS; event publish lag p99 < 2 s.
  - Concurrent-edit fixture: 100% of stale writes rejected with 412.
- **Effort:** 2 weeks.

### V-T4: Search & browse slice
- **Team:** Discovery
- **Decisions implemented:** D17, D11
- **Depends on:** S-T5, S-T6, S-T8
- **Scope:** `search-indexer` + `search-query`; per-cell OpenSearch, index per country, H3 res-5 shard routing; flood control: rating debounce (≤ 1 update/merchant/5 min), salted merchant keys `merchant_id#0..15`, bulk-index backpressure on dedicated ingest nodes; browse (`GET /v1/customer/home`) via customer-bff, fed by catalog/rating contract stubs.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (flag `search_v2` on).
  - Four test levels green; per-salt-ordering contract note merged; dashboards + freshness alert live.
- **Test criteria:**
  - 30k QPS at p99 < 150 ms; ≥ 99% of geo queries touch ≤ 2 shards; freshness p99 < 30 s.
  - 150k-item chain menu update ⇒ feed p99 unchanged (± 10%), reindex < 10 min; hottest salt partition < 2× mean.
- **Effort:** 2 weeks.

### V-T5: Ranking slice
- **Team:** Discovery
- **Decisions implemented:** D17
- **Depends on:** S-T5, S-T8
- **Scope:** `ranking` service re-ranking search top-500 → top-50 with an event-fed feature store; static-ranking fallback flag (doubles as shed-ladder L1); consumes search contract stubs.
- **Definition of Done:**
  - Demo-able end-to-end via the browse BFF endpoint against fakes in the shared E2E env (flag `ranking_ml`, on and off both demoed).
  - Four test levels green; model deploy pipeline documented; SLO + `ownership.yaml`.
- **Test criteria:**
  - Re-rank adds < 50 ms p99; ranking outage ⇒ feed availability ≥ 99.9% via auto-fallback < 10 s.
- **Effort:** 2 weeks.

### V-T6: Feed & merchant-page caches slice
- **Team:** Discovery
- **Decisions implemented:** D11, D17
- **Depends on:** S-T3, S-T8
- **Scope:** Geo-tile feed cache (stale-while-revalidate, CDN-fronted) and merchant-page two-tier cache (in-process singleflight 1 s over Redis 10 s), wired into customer-bff browse/merchant endpoints against search/catalog stubs.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (flag `feed_cache` on).
  - Four test levels green; hit-rate dashboards + stampede alert live.
- **Test criteria:**
  - 1M RPS synthetic on one merchant page ⇒ origin ≤ 1 QPS; cold-tile stampede (10k concurrent) ⇒ exactly 1 origin fetch.
  - Feed cache hit ≥ 85% at peak profile.
- **Effort:** 2 weeks.

### V-T7: Cart slice
- **Team:** Marketplace
- **Decisions implemented:** none (base blueprint, 01 §1)
- **Depends on:** S-T3, S-T5, S-T8
- **Scope:** `cart` service (Redis snapshot + PG), add/remove/get via customer-bff, revalidation against catalog contract (consumes `menu.updated` stub events), ETag concurrency.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (flag `cart_v1` on).
  - Four test levels green; dashboards + alerts live.
- **Test criteria:**
  - Cart ops p99 < 100 ms at 5k RPS; menu-change revalidation reflected < 5 s.
- **Effort:** 1 week.

### V-T8: Pricing & quotes slice
- **Team:** Growth
- **Decisions implemented:** D10
- **Depends on:** S-T3, S-T5, S-T8
- **Scope:** `pricing-promo` quote engine (items, fees, surge, promos, vouchers) via `POST /v1/quotes`; quotes in Redis (10-min TTL) HMAC-signed, PG persistence only at checkout; typed `fees[]`/`discounts[]` line items; consumes cart contract stubs.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env (flag `pricing_v1` on).
  - Four test levels green (pricing math deterministically unit-tested); dashboards + alerts live; key-rotation runbook.
- **Test criteria:**
  - Tampered/expired quote ⇒ 422 on 100% of fixtures; quote p99 < 300 ms at 10k RPS.
  - PG quote writes occur only at checkout (integration-test assertion).
- **Effort:** 2 weeks.

### V-T9: Checkout & order saga slice
- **Team:** Marketplace
- **Decisions implemented:** D22, D9
- **Depends on:** S-T3, S-T5, S-T6, S-T8
- **Scope:** `order` service: state machine (01 §4), saga orchestration against published payment/dispatch/pricing contracts + fakes, idempotent checkout (S-T3 lib), durable timer table + leased sweeper for `T_accept`/`T_dispatch`/capture-by, auto-remediation (PAYMENT_PENDING > 15 min ⇒ void + cancel), bulk-compensation APIs + admin-bff console; `POST /v1/orders`, cancel, get.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: checkout → (sim) payment → accept → deliver (flag `saga_v1` on).
  - Four test levels green incl. every state-machine transition + compensation path; stuck-order SLO dashboard (< 0.05%/day) + alert; console live.
- **Test criteria:**
  - Kill all order pods with 1k pending timers ⇒ 100% fire within 60 s of due.
  - Double "Pay" tap + BFF retry + Kafka redelivery fixture ⇒ exactly one order effect.
  - Remediation fixture auto-voids in < 16 min, exactly once.
- **Effort:** 2 weeks.

### V-T10: Payment authorize/capture/refund slice
- **Team:** Payments
- **Decisions implemented:** D9
- **Depends on:** S-T3, S-T5, S-T6, S-T7, S-T8
- **Scope:** `payment` service against payment-sim (adapter interface, webhooks): authorize/capture/refund + wallet, publishing `payment.*`; PG-durable idempotency on every money mutation; consumes order contract stubs; refund console path via admin-bff.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: auth → capture → refund on payment-sim (flag `payment_v1` on).
  - Four test levels green incl. decline/timeout/webhook-replay fixtures; dashboards (auth rate, failures) + alerts live.
- **Test criteria:**
  - Forced Redis failover during a 1.5× checkout storm ⇒ zero duplicate charges, zero lost orders.
  - Webhook 10× replay ⇒ single state transition; authorize p99 < 500 ms vs sim.
- **Effort:** 2 weeks.

### V-T11: Merchant accept & order-queue slice
- **Team:** Marketplace
- **Decisions implemented:** D7, D11
- **Depends on:** S-T5, S-T6, S-T8
- **Scope:** Merchant incoming-order CQRS read model (sharded by `merchant_id`, projected from `order.*` stub events) + accept/reject via merchant-bff; kitchen-capacity admission tokens (default 30 accepts/10 min, merchant-tunable) inflating quoted prep ETA + busy badge instead of failing checkout; rebuild tooling.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: order appears in queue → accept → saga proceeds (flag `merchant_queue_v1` on).
  - Four test levels green; freshness + admission dashboards; rebuild command executed once.
- **Test criteria:**
  - Queue freshness p99 < 2 s from `order.paid` at 1.5× peak; rebuild of largest cell < 1 h; projection parity 100% on 10k sampled orders.
  - 50× flash-sale sim on one merchant ⇒ zero checkout 5xx; accept rate = configured capacity ± 5%.
- **Effort:** 2 weeks.

### V-T12: Dispatch & driver-offer slice
- **Team:** Logistics
- **Decisions implemented:** D13
- **Depends on:** S-T5, S-T6, S-T7, S-T8
- **Scope:** `dispatch` with zone-owned batch matching: H3-zone single-writer (Kafka partition per zone), 1–2 s tick, greedy-with-swaps, exclusive 10 s driver reservations before offers, deterministic logged snapshots; offer/accept via driver-bff; consumes location + order contract stubs and map-sim ETAs. No first-accept-wins 409 path.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: paid order → offer on driver-bff → accept → assigned (flag `dispatch_batch` on).
  - Four test levels green incl. determinism harness; snapshot log queryable; dashboards + assignment-latency alert live.
- **Test criteria:**
  - Snapshot replay reproduces identical assignments 100%; assignment p95 < 5 s at 1.5× peak-city density.
  - Offer-conflict rate < 0.5%; reservation leak rate 0 in a 24 h soak; sum-of-pickup-ETA ≥ 10% better than greedy baseline on the skewed dataset.
- **Effort:** 2 weeks.

### V-T13: Driver telemetry plane slice
- **Team:** Location
- **Decisions implemented:** D14, D15
- **Depends on:** S-T3, S-T5, S-T7, S-T8
- **Scope:** `location-gateway` (gRPC bidi streams, MQTT fallback, auth-once, 100 ms batching to telemetry topics), H3 res-7 Redis geo index (30 s TTL) with a published kNN read contract for dispatch, Flink 1:10 downsample → Iceberg, PG trip summaries only; driver-bff position-stream endpoint; driver-app protocol migration plan with kill-switch.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoint against fakes in the shared E2E env: simulated driver streams → kNN query returns them (flag `telemetry_v2` on).
  - Four test levels green; ingest/connection/skew dashboards + alerts live; migration playbook published.
- **Test criteria:**
  - 300k msg/s sustained 1 h ⇒ gateway p99 < 5 ms, zero produce errors; 100k reconnect storm recovered < 60 s.
  - kNN p99 < 10 ms at 200k writes/s; hottest H3 key < 2% of writes; PG location writes < 500/s per cell.
- **Effort:** 2 weeks.

### V-T14: Live tracking (realtime gateway) slice
- **Team:** Location
- **Decisions implemented:** D16
- **Depends on:** S-T5, S-T8
- **Scope:** Stateless WebSocket/SSE realtime gateway: per-order channels, 1 msg/3 s throttle, connection-count HPA, graceful drain + resume tokens; customer-bff `GET /v1/orders/{id}/tracking` returns gateway URL + channel token (10 s polling fallback kept); consumes telemetry contract stub events.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoint against fakes in the shared E2E env: live position updates on a simulated order (flag `tracking_push` on).
  - Four test levels green; conns/pod dashboard + drain hooked into rollouts; SLO + `ownership.yaml`.
- **Test criteria:**
  - 2M synthetic connections held; fan-out 650k msg/s sustained.
  - Rolling deploy ⇒ ≥ 99.9% clients resume < 5 s, zero message loss on active orders.
- **Effort:** 2 weeks.

### V-T15: Notifications slice
- **Team:** Growth
- **Decisions implemented:** D23
- **Depends on:** S-T5, S-T6, S-T7, S-T8
- **Scope:** `notification` service on priority-tiered topics (transactional > operational > marketing) with independent consumer groups, per-message-type staleness TTL gates at consume, APNs/FCM provider token buckets — delivered into notify-sink; preference endpoints via customer-bff; consumes order/dispatch/payment stub events.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: order event → push visible in notify-sink (flag `notify_tiers` on).
  - Four test levels green; every message type declares its TTL in `contracts/`; per-tier lag dashboards + alerts.
- **Test criteria:**
  - 30-min pause + 1M backlog resume ⇒ ≥ 99% stale pushes dropped; live transactional p99 < 10 s throughout.
  - Marketing burst ⇒ transactional p99 unchanged (isolation test).
- **Effort:** 2 weeks.

### V-T16: Campaign fan-out slice
- **Team:** Growth
- **Decisions implemented:** D23
- **Depends on:** S-T5, S-T7, S-T8
- **Scope:** Batch campaign pipeline (audience query → rate-shaped send into the marketing-tier contract) with campaign console via admin-bff and per-campaign rate caps; capacity-isolated from transactional consumers; sends land in notify-sink.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: console-created campaign delivers to notify-sink (flag `campaigns_v1` on).
  - Four test levels green; rate-cap dashboards + alerts live.
- **Test criteria:**
  - 20M-recipient simulated campaign ⇒ transactional push p99 unchanged (± 10%); send rate within envelope 100% of the run.
- **Effort:** 2 weeks.

### V-T17: Ratings slice
- **Team:** Growth
- **Decisions implemented:** none (base blueprint, 01 §1)
- **Depends on:** S-T5, S-T6, S-T8
- **Scope:** `rating` service: rate-order endpoint via customer-bff, unlocked by `order.delivered` (stub events), publishes `rating.updated` per contract (search consumes it in its own slice).
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: delivered order → rating accepted → event published (flag `rating_v1` on).
  - Four test levels green; dashboards + alerts live.
- **Test criteria:**
  - Rating before delivery ⇒ 409 on 100% of fixtures; duplicate rating ⇒ idempotent replay.
  - Rate endpoint p99 < 200 ms at 1k RPS.
- **Effort:** 1 week.

### V-T18: Ledger slice
- **Team:** Money Movement
- **Decisions implemented:** D21
- **Depends on:** S-T3, S-T5, S-T6, S-T8
- **Scope:** Standalone append-only double-entry `ledger` service (hash-chained, day+cell partitions) with a published write API (payment/settlement integrate via contract); hourly invariant job (accounts sum to zero; captured − refunded = payables + commission); ledger views via admin-bff.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: simulated payment events post entries, invariants green (flag `ledger_v1` on).
  - Four test levels green; hash-chain verification tool merged; invariant alert live; SLO + `ownership.yaml`.
- **Test criteria:**
  - 7-day soak at 1.5× volume ⇒ hourly invariants show zero drift; hash-chain verify passes.
  - Ledger write p99 < 20 ms.
- **Effort:** 2 weeks.

### V-T19: Settlement, payouts & reconciliation slice
- **Team:** Money Movement
- **Decisions implemented:** D21
- **Depends on:** S-T5, S-T6, S-T7, S-T8
- **Scope:** `settlement` (merchant/driver payout accrual + payout runs, earnings via driver-bff/merchant-bff) and nightly T+1 recon: ingest PSP settlement files (generated by payment-sim), 3-way match (file ↔ ledger contract ↔ order events), break queue with 48 h SLA + console via admin-bff.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: delivered order → accrual → payout report → recon run (flag `settlement_v1` on).
  - Four test levels green; auto-match ≥ 99.5% on prod-shape data; break-aging dashboard + SLA alert; finance runbook.
- **Test criteria:**
  - Seeded 0.5%-discrepancy dataset ⇒ 100% surfaced as breaks, zero silent; recon < 4 h for a 5M-order day.
- **Effort:** 2 weeks.

### V-T20: Risk & abuse slice
- **Team:** Identity & Trust
- **Decisions implemented:** D19
- **Depends on:** S-T3, S-T5, S-T8
- **Scope:** `risk` service scoring between quote and authorize (device fingerprint, velocity, promo-abuse graph, ML score; allow / 3DS step-up / deny — step-up branch published in the order contract), shipped shadow-first; edge abuse controls: endpoint-class token buckets (user/device/IP) + mobile device attestation, signals streamed to risk; case console via admin-bff.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: risky checkout fixture → step-up/deny observable (flag `risk_v1`; shadow and enforce modes both demoed).
  - Four test levels green; abuse dashboards + alerts; retrain pipeline documented.
- **Test criteria:**
  - Scoring adds < 100 ms p99 to the saga; labeled fraud replay: recall ≥ 80% at ≤ 1% false-positive declines.
  - Credential-stuffing sim (10k IPs) blocked ≥ 99% with < 0.1% legit impact; promo-farm sim flagged < 5 min.
- **Effort:** 2 weeks.

### V-T21: PCI capture & vault slice
- **Team:** Payments
- **Decisions implemented:** D18
- **Depends on:** S-T5, S-T7, S-T8
- **Scope:** PSP-hosted-fields card capture in apps/web (no PAN in our stack), isolated CDE account/VPC with `card-vault` (HSM-backed portable tokens) + network-token enrollment; payment-method management via customer-bff; PCI L1 evidence pack + QSA assessment scoped to the CDE.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: add card via hosted-fields sim → token → authorize on payment-sim (flag `vault_v1` on).
  - Four test levels green; PAN scanner in CI; ROC issued with scope = CDE only.
- **Test criteria:**
  - Traffic scanner: zero PAN-shaped payloads outside PSP/CDE domains; tokenize/detokenize p99 < 10 ms.
  - Vault outage sim ⇒ degrade to single-PSP mode, checkout availability ≥ 99.9%; segmentation pen test: zero PAN egress paths.
- **Effort:** 2 weeks.

### V-T22: Multi-PSP routing & auth-window slice
- **Team:** Payments
- **Decisions implemented:** D20
- **Depends on:** S-T5, S-T7, S-T8
- **Scope:** PSP router per (country, method) weighted by rolling auth-rate + fee with auto-failover (5-min error > 3× baseline or auth-rate −5 pts), templated adapter (2-week onboarding), exercised against two payment-sim instances; auth-window management: scheduled orders authorize T−30 min, re-auth job for aging auths, capture-by deadline metric + alert.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: checkout routed across two sims, failover demoed (flag `psp_routing` on).
  - Four test levels green; per-PSP dashboards; failover drill executed; adapter template documented.
- **Test criteria:**
  - Primary-sim kill at 1.5× peak ⇒ auth-success dip < 2% for < 60 s, zero lost/duplicated payments; routing shift visible < 30 s.
  - 24 h auth-window fixture ⇒ re-auth fires, capture succeeds; 7-day soak: zero captures on expired auths.
- **Effort:** 2 weeks.

### V-T23: Cells & global control plane slice
- **Team:** Identity & Trust
- **Decisions implemented:** D1, D3
- **Depends on:** S-T1, S-T5, S-T8
- **Scope:** `user-directory` (`user_id → home cell, jurisdiction`) replicated to every cell, config/flag replication, versioned cell routing-table distribution; directory lookups exposed to BFFs per contract. Async, last-write-wins.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: login fixture resolves its home cell via the directory (flag `directory_v1` on).
  - Four test levels green; replication-lag dashboards + alerts; stale-tolerance failure-mode doc; SLO + `ownership.yaml`.
- **Test criteria:**
  - Cell-local lookup p99 < 10 ms; replication lag p99 < 5 s.
  - Replication halted 10 min ⇒ stale-but-valid serving with zero errors.
- **Effort:** 2 weeks.

### V-T24: Edge routing & second-cell slice
- **Team:** Compute/Delivery
- **Decisions implemented:** D1, D2
- **Depends on:** S-T1, S-T2, S-T8
- **Scope:** Second full cell from IaC, GeoDNS/Anycast, edge cell-router (H3 res-5 / `city_id` → cell from the versioned routing-table contract, header override for tests), city cutover playbook rehearsed with one pilot city.
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoints against fakes in the shared E2E env: same request routed to different cells by geo header (flag `cell_router` on).
  - Second cell passes smoke + E2E; pilot city cut over and rolled back per playbook; router dashboards live.
- **Test criteria:**
  - Cutover + rollback: order success ≥ baseline, zero flip-attributed failures.
  - Router resolution p99 < 2 ms; wrong-cell rate < 0.01%.
- **Effort:** 2 weeks.

### V-T25: Per-cell Kafka & telemetry-cluster slice
- **Team:** Data Platform
- **Decisions implemented:** D5
- **Depends on:** S-T1, S-T5, S-T6
- **Scope:** Per-cell transactional Kafka clusters keyed by `aggregate_id` (dual-publish migration per D30 off `region:aggregate_id`), separate quota-capped telemetry cluster, MM2 restricted to directory/analytics/DR topics; partition counts from the capacity-model contract.
- **Definition of Done:**
  - Demo-able end-to-end via the checkout BFF flow against fakes in the shared E2E env running entirely on the new clusters.
  - Migration complete with deprecation dates on old topics; quotas live; partition derivation in `contracts/`; cluster dashboards + alerts.
- **Test criteria:**
  - Telemetry flood at 2× design (600k msg/s sim) ⇒ transactional publish p99 < 20 ms, unchanged.
  - Per-aggregate ordering property test green across the key migration.
- **Effort:** 2 weeks.

### V-T26: Orders sharding slice
- **Team:** Marketplace
- **Decisions implemented:** D6
- **Depends on:** S-T4, S-T8
- **Scope:** `orders`/`order_events`/outbox onto 256 logical / 4 physical shards by `hash(customer_id)`: dual-write → backfill → verify → flag-gated cutover; cross-shard queries removed (lint rule); includes one 4→8 online remap drill in staging with a timed runbook.
- **Definition of Done:**
  - Demo-able end-to-end via the checkout BFF flow against fakes in the shared E2E env on the sharded store (flag `orders_sharded` on).
  - Parity report published; rollback rehearsed; per-shard dashboards; drill runbook + quarterly calendar.
- **Test criteria:**
  - Checksum parity 100%; cutover window zero 5xx on order APIs; per-shard writes < 100/s at 1.5×; checkout p99 < 800 ms.
  - Remap drill: zero misroutes/write errors at 1× load, total < 4 h.
- **Effort:** 2 weeks.

### V-T27: Payments & ledger sharding slice
- **Team:** Payments
- **Decisions implemented:** D6
- **Depends on:** S-T4, S-T8
- **Scope:** Payment + ledger-accrual tables onto shards by `hash(order_id)` via the same dual-write/backfill/verify/cutover pattern; cross-shard invariant checks wired.
- **Definition of Done:**
  - Demo-able end-to-end via the checkout/capture BFF flow against fakes in the shared E2E env on sharded stores (flag `payments_sharded` on).
  - Parity report; rollback rehearsed; per-shard dashboards + invariant alerts.
- **Test criteria:**
  - Parity 100%; zero failed or duplicated payments during cutover (ledger scan).
  - 7-day soak: hourly cross-shard invariants hold.
- **Effort:** 2 weeks.

### V-T28: Event tiering & replay slice
- **Team:** Data Platform
- **Decisions implemented:** D8
- **Depends on:** S-T6, S-T8
- **Scope:** CDC `order_events` → Iceberg/Parquet; PG keeps 30-day daily partitions (auto-drop); dual-store replayer library (PG + S3 behind one interface) with an order-replay endpoint via admin-bff; Kafka tiered storage (7 d hot / 90 d).
- **Definition of Done:**
  - Demo-able end-to-end via its BFF endpoint against fakes in the shared E2E env: replay an aged order from the lake (flag `event_tiering` on).
  - Pipeline live; partitions auto-drop; lake tables documented; tiering-lag dashboard + alert.
- **Test criteria:**
  - 6-month-old-order replay byte-identical to the golden fixture; tiering lag p99 < 15 min.
  - PG storage plateaus over a 45-day soak.
- **Effort:** 2 weeks.

### V-T29: Purpose-scoped Redis slice
- **Team:** Storage/DB
- **Decisions implemented:** D9, D15
- **Depends on:** S-T1, S-T3
- **Scope:** Split shared Redis into per-cell geo / sessions / cache clusters with fit-for-purpose persistence + eviction; repoint clients via config; decommission the shared instance.
- **Definition of Done:**
  - Demo-able end-to-end via the checkout + tracking BFF flows against fakes in the shared E2E env on the split clusters.
  - Three clusters live per cell; old cluster gone; per-cluster dashboards + policies documented.
- **Test criteria:**
  - Cache FLUSHALL under 1.5× load ⇒ error rate < 0.1%, zero correctness violations (idempotency + money invariants green).
  - Session failover ⇒ forced re-auth for < 5% of active sessions.
- **Effort:** 2 weeks.

### V-T30: Load-shed ladder & waiting-room slice
- **Team:** SRE/Observability
- **Decisions implemented:** D12
- **Depends on:** S-T3, S-T8
- **Scope:** Per-cell overload controller with flag-driven L1–L4 (static ranking → cached feed → checkout waiting room with shown ETA → pause signups), waiting-room service, BFF hooks for all levels — exercised against ranking/cache/checkout contract stubs.
- **Definition of Done:**
  - Demo-able end-to-end via the browse + checkout BFF flows against fakes in the shared E2E env with each ladder level forced (flags `shed_l1..l4`).
  - Four test levels green; ladder-state dashboard; drill runbook.
- **Test criteria:**
  - 8× city-spike sim ⇒ levels engage in order at thresholds; checkout availability ≥ 99.9% for admitted users.
  - De-escalation without flapping (hysteresis verified over 3 cycles).
- **Effort:** 2 weeks.

### V-T31: Capacity model & load-harness slice
- **Team:** SRE/Observability
- **Decisions implemented:** D24
- **Depends on:** S-T2, S-T7
- **Scope:** `capacity-model.yaml` (every 05 §4 dimension, versioned, with derivation doc) + CI job diffing latest load results against it; k6 harness driving 1.5× model numbers at any `BASE_URL`; spatially skewed golden datasets `load-peak-city` and `load-500k-drivers`.
- **Definition of Done:**
  - Demo-able end-to-end via the browse/checkout BFF flows against fakes in the shared E2E env under harness load.
  - Model + CI job live on `main`; datasets versioned in `scenarios/`; harness reads targets from the model.
- **Test criteria:**
  - Fixture result 10% below model ⇒ CI red; baseline ⇒ green.
  - Harness sustains 1.5× largest-cell edge RPS (90k) for 30 min with generator error < 0.01%; skew assertion: top H3 zone ≥ 30% of orders, top merchant ≥ 5%.
- **Effort:** 2 weeks.

### V-T32: Perf cell, driver simulator & shadow-traffic slice
- **Team:** Compute/Delivery
- **Decisions implemented:** D24
- **Depends on:** S-T1, S-T2, S-T7
- **Scope:** Standing prod-scale perf cell from the same IaC (scale-down between runs), 500k-stream driver simulator with adaptive-sampling profiles + reconnect storms, sampled prod-read shadow mirroring (PII-tokenized) to canaries/perf cell; monthly scheduled run archiving results for the capacity CI; load-pass wired into the growth-launch checklist.
- **Definition of Done:**
  - Demo-able end-to-end via the tracking BFF flow against fakes in the shared E2E env fed by the simulator.
  - Perf cell live with monthly run + result archival; simulator in `tools/`; mirroring live with privacy review recorded; launch gate documented.
- **Test criteria:**
  - Simulator sustains 300k msg/s for 1 h with < 0.1% stream drops; 100k reconnect storm recovered < 60 s.
  - Mirroring adds < 0.1% prod latency overhead; seeded +20% canary regression caught by shadow comparison; perf-cell idle cost ≤ 20% of run cost.
- **Effort:** 2 weeks.

### V-T33: DR replication slice
- **Team:** Storage/DB
- **Decisions implemented:** D25
- **Depends on:** S-T1, S-T6
- **Scope:** Tier-0 warm standby to paired cells: PG logical replication per shard, MM2 shadow topics, S3 CRR; lag monitoring; standby sizing per the capacity-model contract; weekly automated read-only promotion dry-run.
- **Definition of Done:**
  - Demo-able end-to-end via the order-detail BFF flow against fakes in the shared E2E env, read from a promoted-standby copy (dry-run mode).
  - All Tier-0 stores replicating for two cell pairs; lag SLO dashboards + alerts; sizing documented.
- **Test criteria:**
  - Replication lag p99 < 5 s at 1.5× peak.
  - Weekly promotion dry-run passes consistency checks 4 consecutive weeks.
- **Effort:** 2 weeks.

### V-T34: DR re-homing & recovery slice
- **Team:** Compute/Delivery
- **Decisions implemented:** D25, D7
- **Depends on:** S-T1, S-T5, S-T8
- **Scope:** Scripted city re-homing (routing-table flip + directory update + replica promotion + DNS) with dry-run, abort path, approvals; in-flight order recovery (saga resume from replicated events, PSP webhook replay via payment-sim, client re-sync in BFF contracts); Tier-1 rebuild-from-events automation (search index, merchant-queue projection, ops index) targeting < 1 h.
- **Definition of Done:**
  - Demo-able end-to-end via the order BFF flows against fakes in the shared E2E env: evacuation drill with live simulated orders completes on the paired cell.
  - Tool merged with dry-run + abort; rebuild automation + progress dashboard; timed runbooks published.
- **Test criteria:**
  - Staging evacuation RTO < 15 min; dry-run emits a full plan with zero mutations; mid-flight abort leaves a verified-consistent state.
  - 1k in-flight-orders drill ⇒ 100% reach terminal state, zero duplicate charges, zero lost payments; largest-cell index rebuild < 60 min, projection parity 100% on 10k aggregates.
- **Effort:** 2 weeks.

### V-T35: Chaos program slice
- **Team:** SRE/Observability
- **Decisions implemented:** D26, D12, D13
- **Depends on:** S-T2, S-T7, S-T8
- **Scope:** The recurring chaos suite against the shared E2E/perf environments under synthetic peak: weekly forced Redis failover during checkout storm + PG primary failover with automated money-invariant assertions, brownout injection (+500 ms on identity/pricing), monthly AZ kill, monthly "monsoon" 8× city-spike drill (demand surge + driver-supply drop) with scorecard, quarterly production cell-evacuation game day with published RTO/RPO.
- **Definition of Done:**
  - Demo-able end-to-end via the checkout BFF flow against fakes in the shared E2E env while a chaos scenario runs (invariants green).
  - Suite scheduled (weekly/monthly/quarterly); assertions automated; results feed the capacity CI; escalation path + scorecard template published.
- **Test criteria:**
  - Every run: zero duplicate charges, zero lost orders; PG failover write-unavailability < 30 s; brownout: checkout p99 < 800 ms with shed active.
  - Monsoon: shed engages < 60 s of breach, dispatch p95 < 5 s in batch mode; game day: RTO ≤ 15 min, RPO ≤ 5 s.
- **Effort:** 2 weeks.

### V-T36: Observability diet slice
- **Team:** SRE/Observability
- **Decisions implemented:** D27
- **Depends on:** S-T3
- **Scope:** 1–5% INFO sampling on read paths (exemplar-linked) via `libs/logging` route classes, full logging for mutations/errors/WARN+/payment/dispatch, tiered retention 3 d hot → 30 d object storage → drop, observability cost dashboard.
- **Definition of Done:**
  - Demo-able end-to-end via any BFF flow against fakes in the shared E2E env: an error `trace_id` resolves across every hop while read-path INFO is sampled.
  - Sampling live fleet-wide; retention policies applied; log-schema tests updated.
- **Test criteria:**
  - Indexed volume < 60k lines/s at peak (from ~600k raw); 100% of error `trace_id`s resolve end-to-end.
  - Observability spend ≥ 60% below the unsampled baseline month.
- **Effort:** 2 weeks.

### V-T37: FinOps slice
- **Team:** SRE/Observability
- **Decisions implemented:** D27, D28
- **Depends on:** S-T1, S-T2
- **Scope:** $/order pipeline (CUR + Kubecost → per-team dashboards beside SLOs, < 24 h lag, budgets against the $0.015/order envelope, 80% alerts), complete CI-enforced `ownership.yaml` (services, topics, dashboards, alerts, budget lines), ≥ 60% stateless compute on spot with PDB-safe eviction, storage lifecycle (Kafka tiered, S3 30 d IA / 180 d Glacier), observability ingest quotas.
- **Definition of Done:**
  - Demo-able end-to-end via the admin-bff cost views against fakes in the shared E2E env.
  - $/order dashboard live; `ownership.yaml` complete with CI enforcement; spot node groups + lifecycle policies applied; monthly review ritual documented with finance.
- **Test criteria:**
  - Unowned-resource fixture ⇒ CI red; seeded 85% burn ⇒ budget alert; $/order reconciles with finance ± 5%.
  - Forced 20% spot eviction at peak ⇒ zero SLO breach; 30-day storage cost trend flattens.
- **Effort:** 2 weeks.

---

## D1–D30 coverage map

| Decision | Implemented by |
|---|---|
| D1 | V-T23, V-T24 |
| D2 | V-T24 |
| D3 | V-T2, V-T23 |
| D4 | V-T1 |
| D5 | V-T25 |
| D6 | S-T4, V-T26, V-T27 |
| D7 | V-T11, V-T34 |
| D8 | S-T6, V-T28 |
| D9 | S-T3, V-T10, V-T29 |
| D10 | V-T8 |
| D11 | V-T4, V-T6, V-T11 |
| D12 | V-T30, V-T35 |
| D13 | V-T12, V-T35 |
| D14 | V-T13 |
| D15 | V-T13, V-T29 |
| D16 | V-T14 |
| D17 | V-T4, V-T5, V-T6 |
| D18 | V-T21 |
| D19 | V-T20 |
| D20 | V-T22 |
| D21 | V-T18, V-T19 |
| D22 | S-T6, V-T9 |
| D23 | V-T15, V-T16 |
| D24 | V-T31, V-T32 |
| D25 | V-T33, V-T34 |
| D26 | V-T35 |
| D27 | V-T36, V-T37 |
| D28 | V-T37 |
| D29 | S-T2 |
| D30 | S-T5 |

**Task count: 45** — 8 ordered setup tasks (S-T1…S-T8) + 37 parallel fullstack
slices (V-T1…V-T37). Every V-task depends only on S-tasks; slices integrate
via versioned contracts + fakes and converge continuously in the shared E2E
env (S-T8). Acceptance: the platform meets the 05 §1 design point when every
slice's test criteria pass at 1.5× the capacity model and the quarterly game
day (V-T35) hits its RTO/RPO targets.
