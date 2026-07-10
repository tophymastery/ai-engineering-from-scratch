# TASKS — 100M-User Scale-Up Roadmap

Implements the decisions **D1–D30** in [docs/05-scale-100m.md](docs/05-scale-100m.md).
Teams are the ownership map of D28. Every task is completable by one team in
≤ 2 weeks; anything larger was split at authoring time.

**Sequencing:** P0–P1 run first and strictly ordered (P1 fixes live correctness
bugs — C3/C4/C10/C20/C21/C23). P2–P6 run largely in parallel by owning team,
respecting per-task `Depends on`. P7 needs P5; P8 needs P1/P3/P4; P9 is
continuous but formalized last.

**Per-task template (normative):**

```markdown
### <PHASE>-T<N>: <Title>
- **Team:** <one team from D28>
- **Decisions implemented:** D<x>[, D<y>]
- **Depends on:** <task IDs | none>
- **Scope:** 2-4 sentences; what is in and explicitly what is out.
- **Definition of Done:** bullets, each independently checkable by someone
  outside the team (merged, deployed-to-env, flag state, dashboard/runbook
  live, ownership.yaml entry, doc updated).
- **Test criteria:** the automated assertions gating the task, with numeric
  pass thresholds; load/chaos criteria cite the capacity model (D24) at 1.5x.
- **Effort:** <= 2 weeks.
```

Conventions applied to every task: load thresholds come from the capacity
model v2 (05 §4) at 1.5×; every money-path chaos criterion asserts **zero
duplicate charges, zero lost orders**; every new service's DoD includes SLO +
dashboard + runbook + `ownership.yaml` entry.

---

## Phase 0 — Foundations & proof of scale

### P0-T1: Capacity model as a CI artifact
- **Team:** SRE/Observability
- **Decisions implemented:** D24
- **Depends on:** none
- **Scope:** Convert the 05 §4 table into a machine-readable `capacity-model.yaml` checked into the repo with a derivation doc; add a CI job that diffs the latest perf-cell results against it. Out of scope: the perf cell itself (P0-T4).
- **Definition of Done:**
  - `capacity-model.yaml` in repo covering every 05 §4 dimension, versioned.
  - CI job on `main` compares newest archived load results to the model and fails on regression.
  - Derivation doc reviewed by Marketplace, Location, Payments leads.
  - `ownership.yaml` entry: SRE/Observability owns the model.
- **Test criteria:**
  - Fixture load-result 10% below model ⇒ CI job red; baseline fixture ⇒ green.
  - Schema validation: model file rejects an unknown/missing dimension.
- **Effort:** 1 week.

### P0-T2: Spatially skewed golden datasets + k6 harness
- **Team:** DevEx
- **Decisions implemented:** D24
- **Depends on:** P0-T1
- **Scope:** Build `load-peak-city` (single-city ×8 skew, celebrity-merchant hot spots) and `load-500k-drivers` golden datasets, plus a k6 harness that drives 1.5× model numbers against any `BASE_URL`. Replaces `load-10k` as the largest scenario.
- **Definition of Done:**
  - Both datasets versioned in `scenarios/`, generated via `seedctl` (seeded, repeatable).
  - Harness in `tools/` with per-dimension target knobs read from `capacity-model.yaml`.
  - Nightly perf-cell invocation documented and scheduled (activates with P0-T4).
- **Test criteria:**
  - Harness sustains 1.5× largest-cell edge RPS (90k) for 30 min with generator-side error < 0.01%.
  - Dataset skew assertion: top H3 zone receives ≥ 30% of orders; top merchant ≥ 5%.
- **Effort:** 2 weeks.

### P0-T3: 500k-stream driver-fleet simulator
- **Team:** Location
- **Decisions implemented:** D24
- **Depends on:** P0-T2
- **Scope:** Simulator holding 500k concurrent gRPC streams with adaptive sampling profiles (1 Hz on-job / 0.1 Hz idle), scripted reconnect storms, spatial movement models. Runs from the perf cell.
- **Definition of Done:**
  - Simulator merged in `tools/`, profile-driven, horizontally scalable.
  - Wired into the `load-500k-drivers` scenario and the nightly perf run.
  - Runbook for operating it during chaos drills.
- **Test criteria:**
  - Sustains 300k msg/s for 1 h with < 0.1% stream drops (simulator-side).
  - Reconnect-storm mode: 100k disconnects issued within 60 s, all re-established < 60 s.
- **Effort:** 2 weeks.

### P0-T4: Standing prod-scale perf cell
- **Team:** Compute/Delivery
- **Decisions implemented:** D24
- **Depends on:** P0-T1
- **Scope:** Stand up a production-sized cell (full stack, prod-shape data volumes, no prod data) used for monthly load runs, weekly chaos (P8-T5), and shadow traffic (P9-T5). Includes cost controls (scale-down between runs).
- **Definition of Done:**
  - Perf cell deployed from the same IaC as prod cells.
  - Monthly scheduled run executes `load-peak-city` and archives results where P0-T1's CI job reads them.
  - Cost note: idle-state spend and per-run spend published.
- **Test criteria:**
  - First full run measures every `capacity-model.yaml` dimension at 1.5× and archives them.
  - Scale-down verified: idle cost ≤ 20% of run cost.
- **Effort:** 2 weeks.

### P0-T5: `libs/sharding` + shard-hint ULIDs
- **Team:** Storage/DB
- **Decisions implemented:** D6
- **Depends on:** none
- **Scope:** Routing library (Go): 256 logical shards → physical cluster map, shard-hint ULID codec (2 hex chars after prefix), remap tool skeleton; reference integration in a sandbox service. Out of scope: migrating real services (P3).
- **Definition of Done:**
  - Library merged with docs; sandbox service uses it end-to-end.
  - Remap tool moves one logical shard between clusters in sandbox.
  - Shard map is config-driven (per-cell overlay), hot-reloadable.
- **Test criteria:**
  - 1M-key routing distribution within 1% of uniform (chi-square).
  - Shard-hint decode agrees with hash routing on 100% of 1M generated IDs.
  - Remap under sandbox write load: zero misroutes, zero write errors.
- **Effort:** 2 weeks.

## Phase 1 — Correctness hotfixes (live bugs; before any scale-out)

### P1-T1: Idempotency rewrite — durable dedupe in the business transaction
- **Team:** Storage/DB
- **Decisions implemented:** D9
- **Depends on:** none
- **Scope:** Rewrite `libs/idempotency`: dedupe = `UNIQUE(idempotency_key)` insert in the caller's own PG transaction (with the business write + outbox row); Redis demoted to read-through response cache + IN_FLIGHT advisory. Wire protocol of 02 §3 (headers, replay semantics) unchanged.
- **Definition of Done:**
  - Library merged; migration helper for adopting services shipped.
  - `Idempotency-Replayed`, `IDEMPOTENCY_KEY_REUSED`, `IDEMPOTENCY_IN_PROGRESS` semantics preserved (contract tests).
  - Docs updated: 05 D9 noted as superseding 02 §3 storage.
- **Test criteria:**
  - 100 concurrent same-key requests ⇒ exactly 1 effect, 99 replays.
  - Redis killed mid-storm ⇒ still exactly 1 effect; p99 penalty of cold cache < +20 ms.
  - Same key + different body ⇒ 409 on 100% of attempts.
- **Effort:** 2 weeks.

### P1-T2: Money-path adoption + Redis-loss chaos gate
- **Team:** Payments
- **Decisions implemented:** D9
- **Depends on:** P1-T1
- **Scope:** Adopt the rewritten library in order checkout, payment authorize/capture/refund, and settlement payouts; delete the Redis-primary path; add the Redis-failover chaos scenario to the recurring suite.
- **Definition of Done:**
  - Three services migrated and deployed to staging + one prod cell.
  - Old Redis-primary code removed from all three.
  - Chaos scenario merged into the weekly suite (feeds P8-T5); runbook updated.
- **Test criteria:**
  - Forced Redis failover during a 1.5× checkout storm ⇒ zero duplicate charges (ledger scan), zero lost orders.
  - Duplicate-effect metric exists and reads 0 over a 7-day staging soak.
- **Effort:** 2 weeks.

### P1-T3: Debezium CDC outbox; pollers deleted
- **Team:** Data Platform
- **Decisions implemented:** D8
- **Depends on:** none
- **Scope:** Debezium log-based CDC connectors for every service's outbox table; delete in-service pollers; time-partition outbox tables with partition-drop cleanup.
- **Definition of Done:**
  - All 12 services publishing via CDC; poller code removed.
  - Partition-drop job scheduled; connector configs in `deploy/`.
  - Relay-lag dashboard + alert per service.
- **Test criteria:**
  - 10k events/s soak for 2 h ⇒ relay lag p99 < 2 s, zero autovacuum alerts.
  - Partition drop reclaims space with zero relay errors or event loss (offset audit).
- **Effort:** 2 weeks.

### P1-T4: Inbox time-partitioning + skip-inbox rule
- **Team:** Data Platform
- **Decisions implemented:** D8
- **Depends on:** none
- **Scope:** Daily-partitioned consumer-inbox tables with 7-day partition drop; audit all consumers and document + apply the skip-inbox rule for naturally idempotent handlers.
- **Definition of Done:**
  - Shared inbox library partitioned; drops automated.
  - Per-consumer inbox/skip decision recorded in `contracts/`.
  - Notification and search projections (highest volume) migrated first.
- **Test criteria:**
  - 10× duplicate-delivery replay burst ⇒ zero duplicate side effects across all consumers.
  - 14-day soak: inbox storage plateaus at ≤ 7 days of volume.
- **Effort:** 2 weeks.

### P1-T5: Durable saga timers + auto-remediation + bulk compensation
- **Team:** Marketplace
- **Decisions implemented:** D22
- **Depends on:** none
- **Scope:** Timer table in the order DB fired by a leased HA sweeper (replaces in-memory timers for `T_accept`, `T_dispatch`, capture-by); auto-remediation of known-safe stuck states (PAYMENT_PENDING > 15 min ⇒ void + cancel); bulk-compensation APIs + console in admin-bff.
- **Definition of Done:**
  - All saga timeouts migrated to durable timers; sweeper leases verified HA.
  - Remediation rules flag-gated and enabled in staging + one prod cell.
  - Bulk-compensation console live; stuck-order SLO dashboard (< 0.05%/day) + alert.
- **Test criteria:**
  - Kill all order pods with 1k pending timers ⇒ 100% fire within 60 s of due time.
  - Remediation fixture: stuck PAYMENT_PENDING auto-voids in < 16 min, exactly once.
  - 7-day staging soak: stuck-order rate < 0.05% of orders.
- **Effort:** 2 weeks.

### P1-T6: DLQ framework + park/inspect/replay CLI
- **Team:** Data Platform
- **Decisions implemented:** D22
- **Depends on:** none
- **Scope:** DLQ topic per consumer group wired into the shared consumer library (park after N retries); `tools/` CLI for park/inspect/replay; depth alerting.
- **Definition of Done:**
  - Library parks poison messages after 3 retries; all consumer groups covered.
  - CLI merged with runbook; DLQ-depth alert per group.
  - Replay path proven against a fixed poison fixture.
- **Test criteria:**
  - Poison message parks without blocking its partition; consumer lag recovers < 60 s.
  - Replay after fix converges exactly-once (inbox dedupe asserted, zero duplicates).
- **Effort:** 1 week.

### P1-T7: Test backdoors compiled out of production
- **Team:** DevEx
- **Decisions implemented:** D29
- **Depends on:** none
- **Scope:** `X-Test-Clock` / `X-Flag-Override` handlers behind a build tag excluded from prod images; gateway strips `X-Test-*` unconditionally (config PR to Compute/Delivery); log-based alert on any appearance in prod. Three independent layers.
- **Definition of Done:**
  - Prod-tagged builds contain no backdoor symbols (CI symbol scan).
  - Gateway strip rule deployed to all cells.
  - Prod-log alert live; docs 03 §5 annotated with the prod-safety note.
- **Test criteria:**
  - CI fails a fixture build that leaks the handler into a prod-tagged image.
  - Header sent to staging-in-prod-mode: echoed absent downstream + alert fires < 1 min.
- **Effort:** 1 week.

### P1-T8: Quotes move to Redis, HMAC-signed
- **Team:** Growth
- **Decisions implemented:** D10
- **Depends on:** none
- **Scope:** Quotes (10-min TTL) stored in Redis and HMAC-signed (body + expiry); checkout verifies signature and expiry; PG persistence only at checkout. Out of scope: pricing logic changes.
- **Definition of Done:**
  - Flag-gated rollout complete in staging + one prod cell; PG quote writes occur only at checkout.
  - Signing keys managed via config with rotation runbook.
  - Pricing dashboard shows quote-store hit/verify metrics.
- **Test criteria:**
  - Tampered quote ⇒ 422 `QUOTE_INVALID`; expired quote ⇒ 422; 100% of fixtures.
  - Staging replay: pricing PG write rate reduced ≥ 95%; checkout E2E green.
- **Effort:** 1 week.

### P1-T9: Topic-versioning schema rules in the registry
- **Team:** Data Platform
- **Decisions implemented:** D30
- **Depends on:** none
- **Scope:** Registry policy: additive-only within a topic; shape changes require a new topic (`<event>.v2`) with a dual-publish window and enforced deprecation date; migrate the 02 §4.3 `schema_version` wording in `contracts/`.
- **Definition of Done:**
  - Registry CI rule active; deprecation-date field mandatory on `.v2` topics.
  - `contracts/` docs updated with one worked `.v2` example.
  - Existing topics audited for latent shape-change risk; findings ticketed.
- **Test criteria:**
  - Fixture PR making an in-place shape change ⇒ registry CI red.
  - `.v2` dual-publish fixture ⇒ both old and new consumers green through the window.
- **Effort:** 1 week.

## Phase 2 — Cells, identity, residency

### P2-T1: Global control plane (user-directory, config/flags, routing table)
- **Team:** Identity & Trust
- **Decisions implemented:** D1, D3
- **Depends on:** none
- **Scope:** `user-directory` service (`user_id → home cell, jurisdiction`) replicated to every cell; config/flag replication; versioned cell routing table distribution. Async, last-write-wins (near-zero churn).
- **Definition of Done:**
  - Directory deployed to two cells with replication; SLO + runbook + `ownership.yaml`.
  - Flags/config replicate cross-cell; routing table versioned and consumed by the edge.
  - Failure-mode doc: stale-tolerant reads specified.
- **Test criteria:**
  - Cell-local directory lookup p99 < 10 ms; replication lag p99 < 5 s.
  - Replication halted 10 min ⇒ cells serve stale-but-valid data with zero errors.
- **Effort:** 2 weeks.

### P2-T2: Identity split — identity-auth / identity-profile
- **Team:** Identity & Trust
- **Decisions implemented:** D3
- **Depends on:** P2-T1
- **Scope:** Split `identity` into `identity-auth` (credentials, tokens, signing keys) and `identity-profile` (per-jurisdiction PII stores); migrate data; in-country stores for ID and VN.
- **Definition of Done:**
  - Both services live; old `identity` retired; contract tests updated.
  - PII rows physically located per jurisdiction (verified against the data inventory).
  - Network policy denies any non-owning-cell access to PII stores.
- **Test criteria:**
  - Migration parity checksums 100%; login/registration E2E green in both cells.
  - Cross-cell profile read succeeds only via the owning cell's API (policy test).
- **Effort:** 2 weeks.

### P2-T3: JWT-at-edge + revocation denylist; introspection deleted
- **Team:** Identity & Trust
- **Decisions implemented:** D4
- **Depends on:** P2-T2
- **Scope:** 15-min ES256 JWTs signed per cell; gateways verify locally with cached JWKS; bloom-filter denylist replicated ≤ 30 s; introspection removed from the hot path (refresh flows still hit identity-auth).
- **Definition of Done:**
  - All gateways on local verification in both cells; introspection endpoints removed from request path.
  - Denylist pipeline live; key-rotation runbook rehearsed.
  - Auth-overhead and revocation-lag dashboards.
- **Test criteria:**
  - Gateway authn adds < 1 ms p99; forged/expired tokens rejected 100%.
  - Revoked token rejected ≤ 30 s later in every cell.
  - identity-auth 10-min outage simulation ⇒ authenticated-traffic error rate unchanged.
- **Effort:** 2 weeks.

### P2-T4: Second cell + cell-router GA + city cutover
- **Team:** Compute/Delivery
- **Decisions implemented:** D1, D2
- **Depends on:** P2-T1, P2-T3
- **Scope:** Stand up a second full cell from IaC; GeoDNS/Anycast to nearest gateway; cell-router GA (H3 res-5 / `city_id` → cell, header override for tests); write and rehearse the city cutover playbook with one pilot city.
- **Definition of Done:**
  - Second cell passes smoke + E2E with golden scenarios.
  - Router GA at the edge; routing table from P2-T1 consumed live.
  - Pilot city cut over (and rolled back once) per playbook.
- **Test criteria:**
  - Cutover + rollback windows: order success rate ≥ baseline, zero failed orders attributed to the flip.
  - Router resolution p99 < 2 ms; wrong-cell request rate < 0.01%.
- **Effort:** 2 weeks.

### P2-T5: Per-cell Kafka, aggregate_id keys, telemetry cluster split
- **Team:** Data Platform
- **Decisions implemented:** D5
- **Depends on:** P2-T4
- **Scope:** Per-cell transactional Kafka clusters; migrate topic keys from `region:aggregate_id` to `aggregate_id` (dual-publish window per D30); separate quota-capped telemetry cluster; MM2 restricted to directory/analytics/DR topics. Partition counts from `capacity-model.yaml`.
- **Definition of Done:**
  - Both cells on their own transactional clusters; key migration complete, old topics deprecated with dates.
  - Telemetry cluster live with quotas; MM2 flows documented and limited.
  - Partition-count derivation checked into `contracts/`.
- **Test criteria:**
  - Telemetry flood at 2× design (600k msg/s simulated) ⇒ transactional publish p99 < 20 ms, unchanged.
  - Per-aggregate ordering property test green across the key migration.
- **Effort:** 2 weeks.

### P2-T6: PII tokenization in events + crypto-shredding erasure + data register
- **Team:** Identity & Trust
- **Decisions implemented:** D3
- **Depends on:** P2-T2
- **Scope:** All events/order snapshots carry `usr_`/`adr_` tokens only; per-user data keys (envelope encryption) in the PII store; erasure API destroys the key; machine-readable data-inventory + retention register, CI-validated.
- **Definition of Done:**
  - PII scanner runs in CI on event fixtures and sampled non-prod logs.
  - Erasure API + runbook live; DPO sign-off recorded.
  - Register checked in; CI fails when a new table/topic lacks a register entry.
- **Test criteria:**
  - Scanner: zero raw PII in golden-traffic events and logs.
  - Erasure fixture: user PII unreadable across stores + backups ≤ 72 h (key destroyed) while order replay still succeeds with tokens.
  - Register-drift fixture (new unregistered table) ⇒ CI red.
- **Effort:** 2 weeks.

## Phase 3 — Data layer scale-out

### P3-T1: Shard orders by hash(customer_id)
- **Team:** Marketplace
- **Decisions implemented:** D6
- **Depends on:** P0-T5
- **Scope:** `orders`, `order_events`, outbox onto 256 logical / 4 physical shards keyed by `hash(customer_id)`: dual-write → backfill → verify → flag-gated cutover. Merchant-facing reads move to P3-T3's model.
- **Definition of Done:**
  - Cutover complete in staging + one prod cell; parity report published.
  - Rollback rehearsed during dual-write phase; per-shard dashboards live.
  - Cross-shard queries removed from the service (lint rule active).
- **Test criteria:**
  - Pre-cutover checksum parity 100% (row counts + sampled content).
  - Cutover window: zero 5xx on order APIs.
  - 1.5× load: per-shard writes < 100/s; checkout p99 < 800 ms.
- **Effort:** 2 weeks.

### P3-T2: Shard payment + ledger tables by hash(order_id)
- **Team:** Payments
- **Decisions implemented:** D6
- **Depends on:** P3-T1
- **Scope:** Same dual-write/backfill/verify/cutover pattern for payments and ledger-accrual tables keyed by `hash(order_id)`.
- **Definition of Done:**
  - Cutover complete in staging + one prod cell; parity report published.
  - Per-shard invariant checks wired (feeds P6-T7); rollback rehearsed.
- **Test criteria:**
  - Parity checksums 100%; zero failed or duplicated payments during cutover (ledger scan).
  - 7-day soak: hourly cross-shard invariants hold (accounts sum to zero).
- **Effort:** 2 weeks.

### P3-T3: Merchant-queue CQRS read model
- **Team:** Marketplace
- **Decisions implemented:** D7
- **Depends on:** P1-T3, P1-T4
- **Scope:** Merchant incoming-order read model sharded by `merchant_id`, projected from `order.*` events; merchant-bff reads it exclusively; rebuild tooling included.
- **Definition of Done:**
  - Read model serving merchant-bff in staging + one prod cell.
  - Direct order-DB merchant queries removed.
  - Rebuild command documented and executed once end-to-end.
- **Test criteria:**
  - Freshness p99 < 2 s from `order.paid` at 1.5× peak.
  - Full rebuild of the largest cell < 1 h; projection vs event-store spot check 100% consistent on 10k orders.
- **Effort:** 2 weeks.

### P3-T4: order_events tiering to Iceberg + dual-store replayer
- **Team:** Data Platform
- **Decisions implemented:** D8
- **Depends on:** P1-T3
- **Scope:** CDC `order_events` → Iceberg/Parquet; PG retains 30-day daily partitions (auto-drop); replayer library reads PG + S3 transparently behind one interface.
- **Definition of Done:**
  - Pipeline live; PG partitions auto-dropped at 30 days.
  - Replayer adopted by the order service (audit/debug path).
  - Lake tables documented and queryable by Data Platform tooling.
- **Test criteria:**
  - Replay of a 6-month-old order byte-identical to its golden fixture.
  - Tiering lag p99 < 15 min; PG storage plateaus over a 45-day soak.
- **Effort:** 2 weeks.

### P3-T5: Purpose-scoped Redis Clusters per cell
- **Team:** Storage/DB
- **Decisions implemented:** D9, D15
- **Depends on:** none
- **Scope:** Split the shared Redis into per-cell clusters: geo, sessions, cache — each with fit-for-purpose persistence and eviction; migrate clients via config; decommission the shared instance.
- **Definition of Done:**
  - Three clusters live per cell; client libraries repointed; old cluster gone.
  - Per-cluster dashboards + eviction/persistence policies documented.
- **Test criteria:**
  - Cache-cluster FLUSHALL under 1.5× load ⇒ error rate < 0.1%, zero correctness violations (idempotency + money invariants green).
  - Session-cluster failover ⇒ forced re-auth for < 5% of active sessions.
- **Effort:** 2 weeks.

### P3-T6: Online resharding drill (4 → 8 physical clusters)
- **Team:** Storage/DB
- **Decisions implemented:** D6
- **Depends on:** P3-T1
- **Scope:** Execute a full 4→8 physical remap in staging under load using the remap tool; harden the tool; produce a timed runbook and schedule the drill quarterly.
- **Definition of Done:**
  - Drill executed end-to-end; tool gaps fixed and merged.
  - Timed runbook published; quarterly calendar entry created.
- **Test criteria:**
  - Zero write errors and zero misroutes during remap at 1× model load.
  - Total drill < 4 h; post-remap placement verification 100%.
- **Effort:** 2 weeks.

## Phase 4 — Telemetry & realtime

### P4-T1: location-gateway streaming ingest tier
- **Team:** Location
- **Decisions implemented:** D14
- **Depends on:** P2-T5
- **Scope:** Per-cell `location-gateway`: gRPC bidi streams (MQTT fallback), auth once per connection, ~64-byte frames, 100 ms batching into telemetry Kafka (key `driver_id`, 512 partitions). Dual-runs with legacy HTTP ingest (removed in P4-T5).
- **Definition of Done:**
  - Tier deployed per cell with connection-count HPA; SLO + runbook + `ownership.yaml`.
  - Driver-app beta cohort on streams; legacy path untouched.
  - Ingest and connection dashboards live.
- **Test criteria:**
  - 300k msg/s sustained 1 h ⇒ gateway p99 < 5 ms, zero Kafka produce errors.
  - 100k-driver reconnect storm recovered < 60 s.
  - Zero per-message auth calls (auth-once verified by trace sampling).
- **Effort:** 2 weeks.

### P4-T2: H3-sharded Redis geo index + dispatch kNN
- **Team:** Location
- **Decisions implemented:** D15
- **Depends on:** P3-T5
- **Scope:** Live positions keyed by H3 res-7 cell (30 s TTL) in the geo Redis cluster; dispatch kNN reads order cell + 6 neighbors; single-GEO-key path removed.
- **Definition of Done:**
  - Dispatch reads the new index in staging + one prod cell; old path deleted.
  - Key-skew dashboard live.
- **Test criteria:**
  - kNN p99 < 10 ms at 200k writes/s.
  - Hottest H3 key < 2% of total writes on `load-500k-drivers`.
  - Candidate-recall parity vs old path ≥ 99.9% on fixtures.
- **Effort:** 2 weeks.

### P4-T3: Flink cold path; PG exits live location
- **Team:** Location
- **Decisions implemented:** D15
- **Depends on:** P4-T1
- **Scope:** Flink job downsamples telemetry 1:10 → Iceberg for analytics/ML; PG reduced to per-trip summary polylines; raw-track PG writes deleted.
- **Definition of Done:**
  - Job live with checkpointing; PG schema slimmed; analytics tables documented.
  - Location PG storage growth curve published.
- **Test criteria:**
  - PG location writes < 500/s per cell.
  - Flink exactly-once verified by replay (record counts match ±0).
  - Lake freshness < 5 min.
- **Effort:** 2 weeks.

### P4-T4: Realtime gateway tier (customer tracking)
- **Team:** Location
- **Decisions implemented:** D16
- **Depends on:** none
- **Scope:** Stateless WebSocket/SSE gateway tier: per-order channels, 1 msg/3 s throttle, connection-count HPA, graceful drain + resume tokens on deploy; BFFs return gateway URL + channel token; 10 s polling fallback retained.
- **Definition of Done:**
  - Tier live; customer tracking migrated off BFF polling in one cell.
  - Drain hooked into Argo Rollouts; capacity dashboard (conns/pod) live.
  - SLO + runbook + `ownership.yaml`.
- **Test criteria:**
  - 2M synthetic connections held; fan-out 650k msg/s sustained.
  - Rolling deploy ⇒ ≥ 99.9% of clients resume < 5 s with zero message loss on active orders.
- **Effort:** 2 weeks.

### P4-T5: Driver protocol migration + legacy decommission
- **Team:** Location
- **Decisions implemented:** D14
- **Depends on:** P4-T1
- **Scope:** Staged driver-app rollout of the streaming protocol with kill-switch; decommission legacy HTTP ingest once adoption > 95%.
- **Definition of Done:**
  - Adoption dashboard; app-store rollout complete.
  - Legacy path removed; 30 incident-free days post-decommission recorded.
- **Test criteria:**
  - Adoption ≥ 95% before decommission.
  - Post-decommission ingest loss rate < 0.01%.
  - Driver-app battery regression < 5% vs baseline cohort.
- **Effort:** 2 weeks.

## Phase 5 — Dispatch & discovery

### P5-T1: Zone-owned batch matcher
- **Team:** Logistics
- **Decisions implemented:** D13
- **Depends on:** P4-T2
- **Scope:** H3-zone single-writer matcher (Kafka partition per zone), 1–2 s tick, greedy-with-swaps batch assignment, deterministic logged snapshots. Legacy greedy stays behind a flag until P5-T2 completes.
- **Definition of Done:**
  - Matcher live in staging + one pilot city.
  - Snapshot log queryable (assignment explainability preserved per 01 §6).
  - Determinism harness in CI.
- **Test criteria:**
  - Snapshot replay reproduces identical assignments 100%.
  - Assignment p95 < 5 s at 1.5× peak-city density.
  - Sum-of-pickup-ETA ≥ 10% better than greedy baseline on `load-peak-city`.
- **Effort:** 2 weeks.

### P5-T2: Driver reservations + offer flow rework
- **Team:** Logistics
- **Decisions implemented:** D13
- **Depends on:** P5-T1
- **Scope:** Exclusive 10 s driver reservations placed before offers; offer flow reworked in dispatch + driver-bff; first-accept-wins `409 OFFER_TAKEN` path and the legacy greedy flag deleted.
- **Definition of Done:**
  - Reservation store live; driver-bff offer card updated.
  - 409 path and greedy code removed.
- **Test criteria:**
  - Offer-conflict rate < 0.5% at peak density.
  - Reservation leak rate 0 in a 24 h soak (TTL reclaim audited).
  - Offer → accept p95 < 3 s.
- **Effort:** 2 weeks.

### P5-T3: Search split + per-cell OpenSearch with H3 routing
- **Team:** Discovery
- **Decisions implemented:** D17
- **Depends on:** P2-T4
- **Scope:** Split `search` into `search-indexer` + `search-query`; per-cell OpenSearch domains, index per country, shard routing by H3 res-5; retire the shared index.
- **Definition of Done:**
  - Both cells on their own domains; old index retired.
  - Shard-routing verified in query profiles; dashboards live.
- **Test criteria:**
  - 30k QPS at p99 < 150 ms.
  - ≥ 99% of geo queries touch ≤ 2 shards (profiler assertion).
  - Index freshness p99 < 30 s.
- **Effort:** 2 weeks.

### P5-T4: Index flood control — debounce, salted keys, bulk backpressure
- **Team:** Discovery
- **Decisions implemented:** D17, D11
- **Depends on:** P5-T3
- **Scope:** Rating-aggregate debounce (≤ 1 doc update/merchant/5 min); salted keys `merchant_id#0..15` on menu/rating topics (dual-publish per D30) with the per-salt-ordering contract note; bulk-index pipeline with backpressure on dedicated ingest nodes.
- **Definition of Done:**
  - Debouncer live; salted topics cut over with deprecation dates on old ones.
  - Ingest nodes isolated from query nodes.
  - Consumer contract note merged in `contracts/`.
- **Test criteria:**
  - 150k-item chain menu update ⇒ feed p99 unchanged (± 10%), reindex completes < 10 min.
  - Hottest salt partition < 2× mean partition load.
  - Rating doc-update rate bounded at merchants/5 min.
- **Effort:** 2 weeks.

### P5-T5: Ranking service + static fallback
- **Team:** Discovery
- **Decisions implemented:** D17
- **Depends on:** P5-T3
- **Scope:** `ranking` service re-ranks OS top-500 → top-50 with an event-fed feature store; static-ranking fallback flag doubling as shed-ladder L1. Model quality iteration out of scope.
- **Definition of Done:**
  - Live in one cell; model deploy pipeline documented.
  - Fallback drill executed; SLO + `ownership.yaml`.
- **Test criteria:**
  - Re-rank adds < 50 ms p99.
  - Ranking outage ⇒ feed availability ≥ 99.9% via auto-fallback < 10 s.
  - Fallback flag flip: availability unchanged (CTR delta measured, non-gating).
- **Effort:** 2 weeks.

### P5-T6: Feed + merchant-page caches with stampede protection
- **Team:** Discovery
- **Decisions implemented:** D11, D17
- **Depends on:** P5-T3
- **Scope:** Geo-tile feed cache (stale-while-revalidate, CDN-fronted) and merchant-page two-tier cache (in-process singleflight 1 s over Redis 10 s).
- **Definition of Done:**
  - Both caches live; hit-rate dashboards; CDN integration for feed tiles.
  - Stampede protection verified in staging.
- **Test criteria:**
  - 1M RPS synthetic on one merchant page ⇒ catalog origin ≤ 1 QPS.
  - Cold-tile stampede (10k concurrent) ⇒ exactly 1 origin fetch.
  - Feed cache hit rate ≥ 85% at peak.
- **Effort:** 2 weeks.

### P5-T7: Discovery load gates in the release pipeline
- **Team:** Discovery
- **Decisions implemented:** D24
- **Depends on:** P5-T3, P5-T4, P5-T5, P5-T6
- **Scope:** Wire discovery load tests at 1.5× model numbers into the release pipeline as a gate for capacity-affecting changes; define the "capacity-affecting" PR class.
- **Definition of Done:**
  - Gate active in CI/CD; PR-class labeling documented.
  - Monthly perf-cell run includes the discovery suite.
- **Test criteria:**
  - Seeded 20% latency-regression fixture ⇒ gate red; baseline ⇒ green.
- **Effort:** 1 week.

## Phase 6 — Money & risk

### P6-T1: PSP-hosted fields; PAN removed from our stack
- **Team:** Payments
- **Decisions implemented:** D18
- **Depends on:** none
- **Scope:** Card capture via PSP-hosted fields/SDKs in apps and web; delete every PAN-touching endpoint from clients, BFFs, and services.
- **Definition of Done:**
  - All card entry via hosted fields; old card endpoints removed.
  - PSP webhook signature verification in place; app releases shipped.
- **Test criteria:**
  - Automated scanner on staging traffic: zero PAN-shaped payloads outside PSP domains.
  - Auth success parity within ± 0.5% of baseline.
- **Effort:** 2 weeks.

### P6-T2: Minimal CDE — card-vault + network tokens
- **Team:** Payments
- **Decisions implemented:** D18
- **Depends on:** P6-T1
- **Scope:** Isolated CDE account/VPC per cell-group containing only `card-vault` (HSM-backed, portable tokens for cross-PSP routing) and PSP connectors; network-token enrollment where issuers support it.
- **Definition of Done:**
  - Vault live; CDE contains exactly vault + connectors (audited).
  - Network tokens active in ≥ 1 market; access controls reviewed.
  - SLO + runbook + `ownership.yaml`.
- **Test criteria:**
  - Tokenize/detokenize p99 < 10 ms.
  - Vault outage simulation ⇒ degrade to single-PSP mode with checkout availability ≥ 99.9%.
  - Pen test: zero PAN egress paths from the CDE.
- **Effort:** 2 weeks.

### P6-T3: Risk service v1 in the saga
- **Team:** Identity & Trust
- **Decisions implemented:** D19
- **Depends on:** none
- **Scope:** `risk` service between quote and authorize: device fingerprint, velocity rules, promo-abuse graph, ML score; verdicts allow / 3DS step-up (new saga branch) / deny. Ships in shadow mode first, then enforcing behind a flag.
- **Definition of Done:**
  - Shadow mode live, then enforcing in one cell (flag documented).
  - 3DS branch added to the saga + state machine, contract-tested.
  - Case-review console in admin-bff; retrain pipeline documented.
- **Test criteria:**
  - Saga overhead < 100 ms p99.
  - Labeled fraud replay: recall ≥ 80% at ≤ 1% false-positive decline rate.
  - 3DS step-up E2E green including the abandonment path.
- **Effort:** 2 weeks.

### P6-T4: Edge abuse controls feeding risk
- **Team:** Compute/Delivery
- **Decisions implemented:** D19
- **Depends on:** P6-T3
- **Scope:** Endpoint-class token buckets (per user/device/IP) on auth, promo, and search classes; mobile device attestation (Play Integrity / App Attest) on risky operations; abuse signals streamed to `risk`.
- **Definition of Done:**
  - Bucket classes live at the gateway; attestation enforced on risky ops.
  - Signal topic produced and consumed by `risk`.
  - Abuse dashboard + runbook.
- **Test criteria:**
  - Credential-stuffing simulation (10k IPs) blocked ≥ 99% with < 0.1% impact on golden legitimate traffic.
  - Promo-farming simulation flagged in `risk` within 5 min.
- **Effort:** 2 weeks.

### P6-T5: Multi-PSP smart routing + failover
- **Team:** Payments
- **Decisions implemented:** D20
- **Depends on:** P6-T2
- **Scope:** Route per (country, method) weighted by rolling auth-rate and fee; auto-failover on 5-min error rate > 3× baseline or auth-rate drop > 5 pts; onboard a second PSP in ≥ 2 markets using a templated adapter.
- **Definition of Done:**
  - Router live; second PSPs onboarded; per-PSP dashboards.
  - Adapter template documented (2-week onboarding target).
  - Failover drill executed.
- **Test criteria:**
  - Primary-PSP kill at 1.5× peak ⇒ auth success dip < 2% for < 60 s; zero lost or duplicated payments.
  - Routing shift observable in metrics < 30 s after breach.
- **Effort:** 2 weeks.

### P6-T6: Auth-window management
- **Team:** Payments
- **Decisions implemented:** D20
- **Depends on:** P1-T5
- **Scope:** Scheduled orders authorize at T−30 min via durable timers (not at scheduling time); re-auth job refreshes auths aging past the PSP window; capture-by deadline metric + alert per order.
- **Definition of Done:**
  - Scheduled-order flow migrated; re-auth job live; state-machine additions contract-tested.
  - Capture-by alert wired to the payments pager.
- **Test criteria:**
  - Fixture crossing a 24 h auth window ⇒ re-auth fires and capture succeeds.
  - 7-day soak: zero captures attempted against expired auths.
  - Capture-by breach alerts < 5 min after deadline.
- **Effort:** 2 weeks.

### P6-T7: Standalone double-entry ledger service
- **Team:** Money Movement
- **Decisions implemented:** D21
- **Depends on:** P3-T2
- **Scope:** Append-only, hash-chained, day+cell-partitioned `ledger` service; `payment` and `settlement` write through its API; hourly invariant job (accounts sum to zero; captured − refunded = payables + commission).
- **Definition of Done:**
  - Ledger live; both writers migrated; hash-chain verification tool merged.
  - Invariant job + alert live; SLO + runbook + `ownership.yaml`.
- **Test criteria:**
  - 7-day soak at 1.5× volume: hourly invariants show zero drift.
  - Hash-chain verification passes over the full soak window.
  - Ledger write p99 < 20 ms.
- **Effort:** 2 weeks.

### P6-T8: T+1 automated reconciliation + break queue
- **Team:** Money Movement
- **Decisions implemented:** D21
- **Depends on:** P6-T7
- **Scope:** Nightly T+1 recon: ingest multi-PSP settlement files, 3-way match (file ↔ ledger ↔ order events), break queue with 48 h SLA and console in admin-bff.
- **Definition of Done:**
  - Recon job scheduled; console live for finance; runbook published.
  - Auto-match ≥ 99.5% demonstrated on production-shape data.
  - Break-aging dashboard + SLA alert.
- **Test criteria:**
  - Seeded 0.5%-discrepancy dataset ⇒ 100% surfaced as breaks, zero silent.
  - Recon completes < 4 h for a 5M-order day.
- **Effort:** 2 weeks.

### P6-T9: PCI-DSS Level 1 assessment (CDE-scoped)
- **Team:** Payments
- **Decisions implemented:** D18
- **Depends on:** P6-T1, P6-T2
- **Scope:** Evidence pack and external QSA assessment scoped to the CDE; remediation of findings; annual compliance calendar.
- **Definition of Done:**
  - ROC issued with scope statement = CDE only.
  - All remediations closed; annual calendar + quarterly scan schedule set.
- **Test criteria:**
  - Segmentation pen test confirms non-CDE networks cannot reach PANs.
  - Quarterly ASV scans clean.
- **Effort:** 2 weeks.

## Phase 7 — Overload defenses & notifications

### P7-T1: Kitchen-capacity admission tokens
- **Team:** Marketplace
- **Decisions implemented:** D11
- **Depends on:** none
- **Scope:** Per-merchant kitchen-capacity token bucket (default 30 accepts/10 min, merchant-tunable) applied in quote + accept; exhaustion inflates quoted prep ETA and shows a busy badge — never a checkout failure.
- **Definition of Done:**
  - Admission live in staging + one prod cell; merchant setting exposed in merchant-bff.
  - Busy badge in customer payloads; dashboards for admission rates.
- **Test criteria:**
  - 50× flash-sale simulation on one merchant ⇒ zero checkout 5xx.
  - Accept rate equals configured capacity ± 5%; ETAs inflate monotonically and recover after the spike.
- **Effort:** 2 weeks.

### P7-T2: Load-shed ladder L1–L4 + waiting room
- **Team:** SRE/Observability
- **Decisions implemented:** D12
- **Depends on:** P5-T5, P5-T6
- **Scope:** Per-cell overload controller with flag-driven levels: L1 static ranking, L2 cached geo-tile feed, L3 checkout waiting room (FIFO token bucket with shown ETA), L4 pause signups; BFF hooks for all levels.
- **Definition of Done:**
  - Controller + waiting-room service live; all four levels wired into BFFs.
  - Drill runbook published; ladder-state dashboard.
- **Test criteria:**
  - 8× city-spike simulation ⇒ levels engage in order at thresholds; checkout availability ≥ 99.9% for admitted users.
  - De-escalation clean: no level flapping (hysteresis verified over 3 cycles).
- **Effort:** 2 weeks.

### P7-T3: Notification priority tiers + staleness gates
- **Team:** Growth
- **Decisions implemented:** D23
- **Depends on:** P1-T4
- **Scope:** Split notification topics into transactional / operational / marketing tiers with independent consumer groups; per-message-type staleness TTL checked at consume; APNs/FCM provider token buckets.
- **Definition of Done:**
  - Tiers live; every message type declares its TTL in `contracts/`.
  - Provider buckets configured; per-tier lag dashboards + alerts.
- **Test criteria:**
  - 30-min consumer pause + 1M backlog resume ⇒ ≥ 99% stale status pushes dropped; live transactional push p99 < 10 s throughout recovery.
  - Marketing burst never delays transactional (isolation test, p99 unchanged).
- **Effort:** 2 weeks.

### P7-T4: Campaign batch fan-out pipeline
- **Team:** Growth
- **Decisions implemented:** D23
- **Depends on:** P7-T3
- **Scope:** Separate batch pipeline for marketing campaigns (audience query → rate-shaped send), capacity-isolated from transactional consumers; campaign console with per-campaign rate caps.
- **Definition of Done:**
  - Pipeline + console live; marketing traffic fully off shared consumers.
  - Rate caps enforced per campaign.
- **Test criteria:**
  - 20M-recipient campaign ⇒ transactional push p99 unchanged (± 10%).
  - Campaign send rate stays within its configured envelope 100% of the run.
- **Effort:** 2 weeks.

### P7-T5: "Monsoon" city-spike drill
- **Team:** SRE/Observability
- **Decisions implemented:** D12, D13
- **Depends on:** P7-T1, P7-T2
- **Scope:** Scripted chaos scenario: 8× single-city demand spike with simultaneous driver-supply drop; monthly schedule; published scorecard (ladder timing, dispatch p95, checkout availability).
- **Definition of Done:**
  - Scenario merged into the chaos suite; monthly calendar entry.
  - First drill executed with scorecard published.
- **Test criteria:**
  - Shed ladder engages < 60 s after threshold breach.
  - Dispatch assignment p95 < 5 s in batch mode during the spike.
  - Zero money-invariant violations during the drill.
- **Effort:** 1 week.

## Phase 8 — DR & chaos

### P8-T1: Tier-0 warm-standby replication
- **Team:** Storage/DB
- **Decisions implemented:** D25
- **Depends on:** P3-T1, P3-T2
- **Scope:** Replicate Tier-0 stores (order, payment, ledger, dispatch, identity-auth) to paired cells: PG logical replication per shard, MM2 shadow topics, S3 CRR; lag monitoring and standby sizing per the capacity model.
- **Definition of Done:**
  - All Tier-0 stores replicating for two cell pairs.
  - Lag SLO dashboards + alerts; standby sizing documented.
  - Weekly automated read-only promotion dry-run scheduled.
- **Test criteria:**
  - Replication lag p99 < 5 s at 1.5× peak.
  - Weekly promotion dry-run passes consistency checks 4 weeks running.
- **Effort:** 2 weeks.

### P8-T2: City re-homing tool
- **Team:** Compute/Delivery
- **Decisions implemented:** D25
- **Depends on:** P8-T1
- **Scope:** Scripted evacuation: routing-table flip + user-directory update + replica promotion + DNS, with dry-run mode, abort path, and approvals workflow.
- **Definition of Done:**
  - Tool merged with dry-run + abort; approvals workflow wired.
  - Staging evacuation executed; timed runbook published.
- **Test criteria:**
  - Staging city evacuation: measured RTO < 15 min.
  - Dry-run emits a complete plan with zero mutations.
  - Mid-flight abort leaves a verified-consistent state.
- **Effort:** 2 weeks.

### P8-T3: In-flight order recovery on failover
- **Team:** Marketplace
- **Decisions implemented:** D25
- **Depends on:** P8-T1
- **Scope:** Saga resume from the replicated event store on a promoted standby; PSP webhook replay closes the ≤ 5 s loss window; client re-sync protocol in BFF contracts. Payments team consulted on webhook replay.
- **Definition of Done:**
  - Resume logic merged; webhook replay tool merged.
  - Client re-sync documented in BFF contracts; drill scenario added to the chaos suite.
- **Test criteria:**
  - Evacuation drill with 1k in-flight orders ⇒ 100% reach a terminal state; zero duplicate charges; zero lost payments.
  - Loss-window orders reconciled < 30 min after failover.
- **Effort:** 2 weeks.

### P8-T4: Tier-1 rebuild-from-events automation
- **Team:** Discovery
- **Decisions implemented:** D25, D7
- **Depends on:** P3-T4
- **Scope:** Automated rebuild of Tier-1 read models in a recovery cell — search index, merchant-queue projection, ops order index — targeting < 1 h; quarterly verification run.
- **Definition of Done:**
  - Rebuild automation merged for all three models; progress dashboard.
  - Quarterly verification scheduled; first run executed.
- **Test criteria:**
  - Timed rebuild of the largest cell's search index < 60 min.
  - Rebuilt projection parity spot check: 100% on 10k sampled aggregates.
- **Effort:** 2 weeks.

### P8-T5: Weekly chaos under synthetic peak
- **Team:** SRE/Observability
- **Decisions implemented:** D26
- **Depends on:** P0-T4, P1-T2
- **Scope:** Weekly automated suite in the perf cell under synthetic peak: forced Redis failover during checkout storm, PG primary failover, brownout injection (+500 ms on identity/pricing), monthly AZ kill; money invariants asserted automatically; results feed the capacity CI.
- **Definition of Done:**
  - Suite scheduled weekly; invariant assertions automated.
  - Results archived where P0-T1's CI job consumes them.
  - Failure escalation path documented.
- **Test criteria:**
  - Every run: zero duplicate charges, zero lost orders.
  - Brownout: checkout p99 < 800 ms with shed ladder active.
  - PG primary failover: write unavailability < 30 s.
- **Effort:** 2 weeks.

### P8-T6: Production cell-evacuation game day
- **Team:** SRE/Observability
- **Decisions implemented:** D25, D26
- **Depends on:** P8-T2, P8-T3
- **Scope:** Quarterly production game day evacuating one real cell to its pair; measured RTO/RPO published against D25 targets; exec observers; gap tickets filed.
- **Definition of Done:**
  - First game day executed; report published with measured numbers.
  - Gaps ticketed with owners; quarterly calendar established.
- **Test criteria:**
  - Measured RTO ≤ 15 min and RPO ≤ 5 s for Tier-0.
  - Customer-visible error-budget burn within the monthly allowance.
- **Effort:** 2 weeks.

## Phase 9 — Cost & steady state

### P9-T1: Log diet — sampling + tiered retention
- **Team:** SRE/Observability
- **Decisions implemented:** D27
- **Depends on:** none
- **Scope:** 1–5% INFO sampling on read paths (exemplar-linked) in `libs/logging` with per-route classes; full logging retained for mutations/errors/WARN+/payment/dispatch; retention 3 d hot → 30 d object storage → drop.
- **Definition of Done:**
  - Sampling live fleet-wide; retention policies applied.
  - Log-schema tests updated; observability cost dashboard live.
- **Test criteria:**
  - Indexed volume < 60k lines/s at peak (from ~600k raw).
  - 100% of error responses' `trace_id`s resolve end-to-end (errors never sampled).
  - Observability spend reduced ≥ 60% vs baseline month.
- **Effort:** 2 weeks.

### P9-T2: Cost-per-order pipeline + ownership enforcement
- **Team:** SRE/Observability
- **Decisions implemented:** D27, D28
- **Depends on:** none
- **Scope:** $/order pipeline (CUR + Kubecost → per-team dashboards beside SLOs); complete `ownership.yaml` covering services, topics, dashboards, alerts, budget lines, CI-enforced; 80% budget alerts; monthly review ritual.
- **Definition of Done:**
  - $/order dashboard live with < 24 h lag; per-team budgets set against the $0.015/order envelope.
  - `ownership.yaml` complete; CI fails unowned resources.
  - Monthly review ritual documented with finance.
- **Test criteria:**
  - Unowned-resource fixture ⇒ CI red.
  - Seeded 85%-burn scenario ⇒ budget alert fires.
  - $/order reconciles with finance actuals ± 5%.
- **Effort:** 2 weeks.

### P9-T3: Spot compute + storage lifecycle + ingest quotas
- **Team:** Compute/Delivery
- **Decisions implemented:** D27
- **Depends on:** none
- **Scope:** ≥ 60% of stateless compute on spot with PDB-safe eviction handling; Kafka tiered storage (7 d hot / 90 d tiered); S3 lifecycle (30 d → IA, 180 d → Glacier); observability ingest quotas alerting at 80%.
- **Definition of Done:**
  - Spot node groups live for BFFs and stateless services; lifecycle policies applied.
  - Quotas configured; savings report published.
- **Test criteria:**
  - Forced 20% spot eviction at peak ⇒ zero SLO breach.
  - 30-day storage cost trend flattens post-lifecycle.
  - Seeded ingest overrun ⇒ quota alert fires.
- **Effort:** 2 weeks.

### P9-T4: Shared multi-tenant preview infrastructure
- **Team:** DevEx
- **Decisions implemented:** D29
- **Depends on:** none
- **Scope:** Replace per-PR full stacks: shared baseline stack + per-PR deploy of changed services + dependents with header routing (reusing the `run_id` isolation pattern of 03 §4); scale-to-zero after 2 h idle; 7-day TTL.
- **Definition of Done:**
  - ApplicationSet migrated; per-PR URL behavior preserved; old full stacks removed.
  - E2E gate runs unchanged against shared previews.
- **Test criteria:**
  - Preview cost per PR reduced ≥ 80%.
  - E2E gate runtime within ± 10% of baseline.
  - Cross-PR isolation: two PRs mutating the same entity type show zero data bleed.
- **Effort:** 2 weeks.

### P9-T5: Shadow traffic + quarterly capacity gate
- **Team:** SRE/Observability
- **Decisions implemented:** D24
- **Depends on:** P0-T4
- **Scope:** Mirror sampled production reads (PII-tokenized per D3) to canaries and the perf cell; quarterly capacity-model refresh ritual; wire "load-test pass at model numbers" into the growth-launch checklist as a release gate.
- **Definition of Done:**
  - Mirroring live with sampling config; privacy review recorded.
  - Quarterly refresh scheduled; gate present in the launch checklist.
- **Test criteria:**
  - Mirroring adds < 0.1% overhead to production request latency.
  - Seeded +20% latency regression on a canary caught by shadow comparison before rollout.
  - Quarterly refresh emits a CI-consumable model update (P0-T1 job consumes it).
- **Effort:** 2 weeks.

---

**Task count: 63** (P0: 5, P1: 9, P2: 6, P3: 6, P4: 5, P5: 7, P6: 9, P7: 5, P8: 6, P9: 5).
Acceptance: the platform meets the 05 §1 design point when every task's test
criteria pass at 1.5× the capacity model and the quarterly game day (P8-T6)
hits its RTO/RPO targets.
