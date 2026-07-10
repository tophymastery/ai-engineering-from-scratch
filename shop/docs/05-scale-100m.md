# 05 — Scale: 100M Users, 5M Orders/Day, 500k Drivers

This document takes the platform in docs 01–04 (sound at ~1M orders/day,
single region) to production grade at 100M registered users. It is the result
of two independent designs (Agent-A architecture pass, Agent-B SRE critique)
reconciled into one decision log. **Where this doc conflicts with docs 01–04,
this doc supersedes them** (notably: idempotency storage, Kafka keying,
dispatch matching, capacity numbers, event-schema evolution, log volume).

## 1. Design point

| Dimension | Number |
|---|---|
| Registered users / DAU | 100M / ~20M |
| Orders | 5M/day; **peak 580 created/s** (×10 peak-hour skew); ~1.8B/yr |
| Concurrently active orders at peak | ~1.4M |
| Concurrent drivers | 500k; GPS ingest design load **300k msg/s** (range 125–500k) |
| Long-lived realtime connections | ~2M (1.4M tracking watchers + 500k drivers + merchant tablets) |
| Countries | 6 (data-residency obligations in ID and VN; PDPA/GDPR-class elsewhere) |
| Edge traffic | ~250k RPS peak global; largest cell ~60k RPS |

## 2. Reconciliation

Verdicts on Agent-B findings C1–C23. ACCEPT = folded into the decision log;
MERGE = independently converged in the round-1 design (confirmation, details
absorbed); REJECT = not adopted. **Result: 19 ACCEPT (1 partial), 4 MERGE, 0 REJECT.**

| # | Sev | Verdict | Resolution |
|---|---|---|---|
| C1 | blocker | MERGE | Round-1 already chose cells + city single-homing (D1/D2). Adopted B's framing: cell = full self-contained stack, thin global control plane. |
| C2 | blocker | ACCEPT | Capacity re-baselined to ×10 skew ⇒ 580 orders/s peak; model is a checked-in artifact CI-diffed against load tests → D24, §4. |
| C3 | blocker | ACCEPT | **Round-1 D9 was wrong.** Durable dedupe = `UNIQUE(idempotency_key)` in the service's own PG, inserted in the same transaction as the business write. Redis is read-through cache + IN_FLIGHT advisory only; losing it degrades latency, never correctness → D9. |
| C4 | blocker | ACCEPT | Outbox relay = Debezium log-based CDC only (no pollers); outbox + consumer-inbox tables time-partitioned, cleanup = partition drop (7-day inbox); inbox skipped where handlers are naturally idempotent. Event tiering to Iceberg was already round-1 D8; the rest is new → D8. |
| C5 | blocker | MERGE | Round-1 D12/D13 already had streaming ingest + adaptive sampling + H3-sharded Redis. Absorbed: auth once per connection, MQTT fallback, telemetry as an isolated plane → D14/D15. |
| C6 | blocker | ACCEPT (partial) | Risk service accepted in full → D19 (new). PCI: adopt PSP-hosted fields so clients/apps never touch PANs; **reject "PSP tokenization only ⇒ SAQ-A"** — PSP-locked tokens break multi-PSP routing (D20), so a minimal CDE vault is retained for token portability → D18. |
| C7 | blocker | ACCEPT | PII store per cell/jurisdiction; events carry `usr_` tokens, never PII; erasure = crypto-shredding via per-user data keys; checked-in data-inventory + retention register validated in CI → D3. |
| C8 | major | ACCEPT | Kafka per cell keyed by `aggregate_id` alone (region prefix is redundant once clusters are per-cell); telemetry on a separate cluster; partition counts derived from the capacity model → D5. |
| C9 | major | ACCEPT | Round-1 had batch matching only as an overload mode — B is right that greedy + first-accept-wins melts at density. Zone-owned batch matching becomes the **default**: H3-zone single-writer, 1–2 s tick, exclusive short-TTL driver reservations before offers → D13. |
| C10 | major | ACCEPT | Durable timer table (leased sweeper), DLQ per consumer group with park/inspect/replay CLI, admin bulk compensation, auto-remediation of known-safe stuck states → D22 (new). |
| C11 | major | ACCEPT | Ledger + T+1 recon converged (round-1 D18); absorbed the auth-window gap: scheduled orders authorize at T−30 min; re-auth job for aging auths; capture-by deadline monitored → D20/D21. |
| C12 | major | ACCEPT | Priority-tiered notification topics, per-type staleness TTL gates on consume, provider token buckets, campaigns on a separate batch path → D23 (new). |
| C13 | major | MERGE | Round-1 D14 already added a push tier. Absorbed: connection-count scaling, graceful drain on deploy, BFFs hand out gateway URL + token and never hold sockets → D16. |
| C14 | major | MERGE | Independent convergence: short-lived JWTs verified at edge + replicated bloom denylist; introspection path deleted → D4. |
| C15 | major | ACCEPT | Rating aggregates debounced per merchant; bulk-index pipeline with backpressure on dedicated ingest nodes; stampede-protected geo-tile feed cache → D17. |
| C16 | major | ACCEPT | INFO sampled 1–5% on read paths (exemplar-linked); full logging only for mutations/errors/payment/dispatch; tiered retention 3 d hot → 30 d object storage → drop → D27. |
| C17 | major | ACCEPT | Standing prod-scale perf cell, 500k-stream driver simulator, spatially skewed datasets, shadow traffic; load-test pass at model numbers gates capacity-affecting releases → D24. |
| C18 | major | ACCEPT | Weekly forced Redis/PG failovers under synthetic peak with money invariants (zero duplicate charges, zero lost orders); brownout latency injection (+500 ms on identity/pricing); quarterly production cell evacuation → D26. |
| C19 | minor | ACCEPT | Shared multi-tenant preview infra (per-PR prefixes over a shared baseline, deploy changed services only, scale-to-zero) → D29. |
| C20 | minor | ACCEPT | `X-Test-Clock`/`X-Flag-Override` compiled out of prod builds (build tag) AND stripped unconditionally at gateway AND alert if seen in prod logs → D29. |
| C21 | minor | ACCEPT | Quotes live in Redis (10 min TTL), HMAC-signed for checkout verification; persisted to PG only at checkout → D10. |
| C22 | minor | ACCEPT | Endpoint-class token buckets per user/device/IP, device attestation on mobile, abuse signals feed the risk service → D19. |
| C23 | minor | ACCEPT | Schema rule fixed: additive-only within a topic; genuine shape changes get a new topic (`order.paid.v2`) with dual-publish window + registry-enforced deprecation date. Supersedes the `schema_version`-bump wording in 02 §4.3 → D30. |

## 3. Final decision log

### 3.A Topology, residency, edge

**D1: Cell-based topology; globally active-active, locally single-homed.**
- Decision: One full self-contained cell (all services, PG shards, Kafka, Redis, OpenSearch, K8s) per country or mega-city, capped at ≤750k orders/day; every city is homed to exactly one cell; the entire order path executes in-cell with zero cross-cell calls. A thin global control plane carries only: identity-auth signing keys, config/flags, user-directory, cell routing table.
- Rationale: Food delivery is intrinsically local; single-homing gives single-region latency and correctness, cells cap every sizing problem and bound blast radius to ≤15% of a country.
- Consequence: Failover = re-home cities (D25), not replicate writes. Split a cell at 70% capacity.

**D2: Edge cell-router.** GeoDNS/Anycast → nearest gateway; router resolves lat/lng (H3 res-5) or `city_id` → cell via the replicated routing table; header override for tests. Consequence: staging/preview model one cell, keeping docs-03 tooling valid.

**D3: Data residency, PII tokenization, crypto-shredding.**
- Decision: PII (name, phone, address, KYC) lives only in a dedicated per-cell PII store in the user's jurisdiction (in-country for ID/VN). Everything else — including **all events and order snapshots** — carries `usr_`/`adr_` tokens only. Erasure = destroy the per-user data-encryption key (crypto-shredding), making immutable events/backups unreadable. A machine-readable data-inventory + retention register is checked in and CI-validated like the log schema.
- Rationale: PDP Law (ID) / Decree 53 (VN) mandate residency; right-to-erasure is impossible against immutable event stores without envelope encryption.
- Consequence: `identity` splits into `identity-auth` (global keys/credentials) and `identity-profile` (per-cell PII); admin tooling fetches PII from the owning cell.

**D4: Stateless auth at the edge.** 15-min ES256 JWTs signed per cell, verified locally with cached JWKS; revocation via a replicated bloom-filter denylist refreshed ≤30 s; refresh tokens hit identity-auth. Rationale: 250k RPS × introspection makes identity the global SPOF. Consequence: ≤30 s revocation lag, identity availability detached from checkout SLO. (Convergent with C14.)

**D5: Kafka per cell; telemetry isolated.** One transactional Kafka cluster per cell (KRaft, 3-AZ, RF=3, `acks=all`), topics keyed by `aggregate_id` alone; a **separate** telemetry cluster per cell for `driver.location` (quotas so telemetry can never starve money events); partition counts derived from §4. Cross-cell replication (MM2) only for user-directory, analytics-to-lake, and DR shadow topics. Consequence: no global bus on the order path; supersedes the `region:aggregate_id` keying in 01 §3 / 02 §4.3.

### 3.B Data layer

**D6: Application-level sharding over plain PostgreSQL.** `libs/sharding` with 256 logical shards per cell mapped to N physical clusters (start N=4; split by remapping, tooled + drilled). Shard keys: orders by `hash(customer_id)`; payment/ledger by `hash(order_id)`; identity by `hash(user_id)`. Prefixed ULIDs embed a 2-hex-char shard hint. Rationale: peak ~29k writes/s global (50 writes/order × 580/s) ⇒ ≤8k/s per cell ⇒ <100/s per shard — the problem is storage growth, vacuum, and fault isolation, not TPS; plain PG keeps outbox transactionality. Consequence: cross-shard queries forbidden; non-shard-key access paths become read models (D7).

**D7: CQRS read models for second access paths.** Merchant order queue (sharded by `merchant_id`, built from `order.*` events, freshness p99 < 2 s), ops/admin order search (OpenSearch headers index), customer history (shard-local). Consequence: projections must be rebuildable from the event store (tested, P8-T4).

**D8: Event-volume hygiene: CDC outbox, partition-drop cleanup, tiering.**
- Decision: Outbox relay = Debezium log-based CDC only (pollers banned — vacuum storms at this churn). Outbox and consumer-inbox tables are time-partitioned; cleanup = partition drop (inbox retains 7 days). Inbox is skipped where the handler is naturally idempotent (documented per consumer). `order_events`: 30 days hot in PG (daily partitions per shard), CDC → Iceberg/Parquet for the tail (~30B rows/yr); replayer reads PG+S3 transparently. Kafka tiered storage: 7 d hot, 90 d tiered.
- Consequence: "replay any order" spans two stores behind one interface.

**D9: Idempotency: durable dedupe in the database, Redis as cache only.** *(Supersedes 02 §3 storage note — round-1 design corrected per C3.)*
- Decision: The source of truth for effect-once is a `UNIQUE(idempotency_key)` insert in the **service's own PG, in the same transaction as the business write and outbox row**. Redis holds only a read-through response cache and an IN_FLIGHT advisory marker for fast double-tap rejection.
- Rationale: Async-replicated Redis loses acked SETNX on failover and evicts DONE records; at 580 checkouts/s one failover is thousands of potential double charges. A unique constraint cannot lose.
- Consequence: Redis loss degrades latency (uncached replays hit PG), never correctness — asserted weekly by chaos (D26). Wire format of 02 §3 (headers, replay semantics) is unchanged.

**D10: Quotes in Redis, HMAC-signed.** Quotes (10-min TTL, ~99% never used) live in Redis, signed (HMAC over quote body + expiry) so checkout can verify integrity; persisted to PG only at checkout. Consequence: pricing's PG write load drops ~50×; a Redis flush merely forces re-quote.

### 3.C Hot partitions, overload, dispatch

**D11: Celebrity-merchant defenses.** (a) Merchant-keyed fan-out topics use salted keys `merchant_id#(0..15)` — per-salt ordering suffices for last-write-wins projections; (b) merchant/store pages served from two-tier cache (in-process singleflight-coalesced 1 s TTL over Redis 10 s TTL) ⇒ a 1M-RPS merchant page costs catalog ≤1 QPS; (c) per-merchant kitchen-capacity admission tokens (default 30 accepts/10 min, merchant-tunable) — exhaustion inflates quoted prep ETA and shows "busy" instead of failing checkout.

**D12: City-wide surge: explicit load-shed ladder.** Regional overload controller with flag-driven levels: L1 drop ML re-ranking (static ranking) → L2 serve cached geo-tile feed → L3 checkout waiting room (FIFO token bucket, ETA shown) → L4 pause signups. Surge pricing (pricing-promo) is the economic valve. Drilled monthly (P7-T5).

**D13: Dispatch = zone-owned batch matching, always.** *(Supersedes 01's per-order greedy scoring; round-1 "batch only under load" corrected per C9.)*
- Decision: H3 zones, each owned by a single writer (Kafka partition per zone); every 1–2 s tick, batch-assign all pending orders in the zone to candidate drivers (greedy-with-swaps; Hungarian for small batches); driver gets an **exclusive short-TTL reservation** (10 s) before the offer, eliminating first-accept-wins 409 storms. Each batch logs its full input snapshot ⇒ deterministic and explainable (preserves 01 §6).
- Rationale: Per-order greedy concentrates offers on the same top drivers and degrades exactly when supply is scarce.
- Consequence: assignment p95 budget stays ≤5 s (tick ≤2 s + offer ≤3 s); one matching code path instead of two.

### 3.D Location & realtime

**D14: Telemetry ingest plane.** Drivers hold one persistent gRPC bidi stream (MQTT fallback) to a per-cell `location-gateway` tier — auth once per connection, ~64-byte binary frames, adaptive sampling 1 Hz on-job / 0.1 Hz idle ⇒ design 300k msg/s global; gateways batch 100 ms windows into the telemetry Kafka cluster (key `driver_id`, 512 partitions/cell). No per-ping HTTP, no BFF involvement.

**D15: H3 geo store; PG exits the live path.** Live positions in Redis Cluster keyed by H3 res-7 cell (30 s TTL); dispatch kNN = order's cell + 6 neighbors. Cold path: Flink downsamples 1:10 → Iceberg for analytics/ML; PG keeps per-trip summary polylines only. Rationale: one GEO key is itself a hot partition at 500k drivers; PG at full rate is petabyte nonsense.

**D16: Dedicated realtime gateway tier.** Stateless WebSocket/SSE gateways (per-order channels, 1 update/3 s throttle) sized on **connection count** (~50k conns/pod), with graceful drain + client reconnect-with-resume on deploy; BFFs return a gateway URL + channel token and never hold sockets. Handles ~2M connections; driver ingest stays on the telemetry plane (D14). Consequence: canary rollouts no longer sever the fleet.

### 3.E Search & discovery

**D17: Per-cell OpenSearch, geo routing, flood control, two-phase ranking.** Index per country, shard routing by H3 res-5 ⇒ a query touches 1–2 shards; ~30k QPS peak in the largest cell ⇒ ~24 data nodes + dedicated ingest nodes. Rating aggregates debounced (≤1 doc update / merchant / 5 min); bulk-index pipeline with backpressure so a 150k-item chain-menu update never contends with feed reads; geo-tile feed cache with stampede protection (singleflight + stale-while-revalidate). Retrieval (top-500) is OS; a separate `ranking` service ML-re-ranks the top 50, with a static-ranking fallback flag (= shed ladder L1).

### 3.F Payments, risk, money movement

**D18: PCI scope: PSP-hosted fields + minimal vault.** Card capture via PSP-hosted fields / SDK ⇒ PANs never touch our clients, BFFs, or services. A minimal CDE (isolated account/VPC per cell-group) contains only `card-vault` (HSM-backed) holding portable tokens so routing can move a card between PSPs (D20); network tokens preferred where issuers support them. PCI-DSS Level 1 assessment scoped to the CDE (2 components). Partial-reject rationale vs C6: PSP-only tokenization locks each card to one PSP and kills failover routing.

**D19: Risk service in the saga; edge abuse controls feed it.** New `risk` service between quote and authorize: device fingerprint, velocity rules, promo-abuse graph, ML score; outcomes = allow / 3DS step-up (saga branch) / deny. Edge: endpoint-class token buckets per user/device/IP + mobile device attestation; abuse signals stream to `risk`. Sizing: 0.5% fraud at 5M orders/day = 25k bad orders/day — a service, not a rule in a BFF.

**D20: Multi-PSP routing + auth-window management.** ≥2 PSPs per country; routing weighted by rolling auth-rate and fee; automatic failover when a PSP's 5-min error rate >3× baseline or auth rate drops >5 pts. Auth lifetimes are managed: scheduled orders authorize at T−30 min (not at scheduling time); a re-auth job refreshes auths aging past the PSP window; every order has a capture-by deadline metric with alerting.

**D21: Double-entry ledger + automated reconciliation.** Standalone append-only, hash-chained `ledger` service (partitioned day+cell); payment and settlement write through it. Nightly T+1 recon ingests PSP settlement files and matches file ↔ ledger ↔ order events: ≥99.5% auto-match; breaks land in an SLA'd queue (48 h); hourly invariants (all accounts sum to zero; captured − refunded = payables + commission) alert on any nonzero drift. 0.01% silent drift = 500 orders/day — only invariants catch it.

**D22: Saga operability: timers, DLQ, remediation.** Saga timeouts (`T_accept`, `T_dispatch`, capture-by) are rows in a durable timer table in the order DB, fired by a leased sweeper (replay-safe, survives pod kill). Every consumer group has a DLQ with a park/inspect/replay CLI. Known-safe stuck states auto-remediate (e.g. PAYMENT_PENDING >15 min ⇒ void auth + cancel); the rest hit an admin bulk-compensation console. SLO: stuck orders (>30 min without transition) <0.05% of daily orders, alert on breach.

### 3.G Notifications

**D23: Notification tiers, staleness gates, campaign isolation.** Three topic tiers — transactional (order/payment) > operational (driver/merchant) > marketing — with independent consumer groups and provider token buckets (APNs/FCM). Every message type declares a staleness TTL checked at consume time: after a 30-min outage, ~1M backlogged status pushes are dropped as stale instead of replayed over live traffic. Marketing campaigns use a separate batch fan-out pipeline that can never occupy transactional capacity.

### 3.H Capacity & performance engineering

**D24: Capacity model as a CI artifact; prod-scale perf cell; shadow traffic.** The §4 table lives in the repo, versioned; CI diffs its numbers against the latest load-test results and fails on regression. A standing prod-sized perf cell runs monthly: 500k-stream driver simulator, spatially skewed order generation (`load-peak-city`, `load-500k-drivers` golden datasets), PG failover under load, consumer rebalance under lag. Sampled prod reads are shadow-mirrored to canaries/perf cell. Passing at model numbers is a release gate for capacity-affecting changes.

### 3.I DR & chaos

**D25: Tiered DR; city re-homing is the failover unit.** Each cell pairs with a recovery cell (async logical replication for PG, MM2 shadow topics, S3 CRR). **Tier 0** (order, payment, ledger, dispatch, identity-auth, location hot path): RPO ≤5 s, RTO ≤15 min via scripted re-homing (routing-table flip + replica promotion). **Tier 1** (search, projections, notification, rating): rebuild from events, RPO ≤5 min, RTO ≤1 h. **Tier 2** (analytics/ML): RPO 24 h. In-flight orders: saga resumes from replicated events; the ≤5 s loss window is closed by client re-sync + PSP webhook replay — money is never lost (worst case an auth void). Standing cost: ~25–30% overhead, budgeted in D27.

**D26: Chaos that matches the failure modes.** Weekly, under synthetic peak in the perf cell: forced Redis failover during checkout storm and PG primary failover — pass requires **zero duplicate charges, zero lost orders** (asserts D9/D22 invariants); brownout injection (+500 ms on identity/pricing) validating deadline budgets and shed ladder; AZ kill monthly; production cell evacuation quarterly with measured RTO/RPO published against D25 targets.

### 3.J Cost, org, engineering hygiene

**D27: Unit-economics budget: infra ≤ $0.015/order.** (~$75k/day at 5M orders.) Mechanisms: per-team showback (Kubecost + CUR) beside SLO dashboards, 80% budget alerts; HPA ceilings derived from §4 (no unbounded autoscaling); ≥60% stateless compute on spot with PDB-safe eviction; storage lifecycle (Kafka tiered, S3 30 d→IA, 180 d→Glacier); **observability diet per C16**: INFO sampled 1–5% on read paths (exemplar-linked), full logs only for mutations/errors/WARN+/payment/dispatch, retention 3 d hot → 30 d object store → drop (raw ~600k lines/s → <60k/s indexed); new always-on infra requires a $/order note.

**D28: Ownership map.** Domain teams: Marketplace (order, cart, saga), Payments (payment, card-vault, PSP adapters), Money Movement (ledger, settlement, recon), Logistics (dispatch), Location (telemetry gateway, tracking, realtime gateway), Discovery (catalog, search, ranking), Growth (pricing-promo, notification, rating), Identity & Trust (identity-auth/profile, user-directory, risk). Platform teams: Compute/Delivery, Data Platform, Storage/DB, SRE/Observability, DevEx. Every repo path, topic, dashboard, alert, and budget line names exactly one owner in a machine-readable `ownership.yaml` (CI-enforced). ~13 teams, you-build-it-you-run-it.

**D29: Test-infra safety & economics.** Test backdoors (`X-Test-Clock`, `X-Flag-Override`) are compiled out of production builds (build tag) **and** stripped unconditionally at the gateway **and** alarmed if seen in prod logs — three independent layers. Preview envs become shared multi-tenant: one baseline stack, per-PR deploy of changed services + dependents with header routing (the `run_id` isolation of 03 §4 already proves the pattern), scale-to-zero after 2 h idle, 7-day TTL.

**D30: Event schema evolution (supersedes 02 §4.3 wording).** Additive-only within a topic — optional fields, never rename/repurpose, no shape changes under a `schema_version` bump. A genuine shape change is a new topic (`order.paid.v2`) with a dual-publish window and a registry-enforced deprecation date for the old topic.

## 4. Capacity model (v2 — the checked-in numbers)

| Dimension | Number | Derivation / consequence |
|---|---|---|
| Orders | 5M/day; peak 580/s global; largest cell ~150/s | ×10 peak-hour skew (C2) |
| Concurrent active orders | ~1.4M at peak | sizes realtime channels, dispatch zones, timer sweeper |
| Edge traffic | ~250k RPS peak global; largest cell ~60k RPS | ~50 calls/order session, 10:1 browse:order; CDN offloads ~40% of feed reads |
| BFF fleet | customer-bff ~120 pods/largest cell | 500 RPS/pod @ p99<300 ms, 40% headroom |
| DB writes | ~29k/s global peak (50 writes/order); ≤8k/s per cell; <100/s per logical shard | 256 shards on 4 PG clusters/cell; growth driver is storage + vacuum, not TPS |
| order_events | ~30B rows/yr, ~190 GB/day raw | 30 d hot PG partitions → Iceberg (D8) |
| Kafka transactional | ~9k events/s peak per busiest cell | 15 events/order; 3-AZ RF=3 cluster per cell |
| Kafka telemetry | 300k msg/s global design (range 125–500k); ~20 MB/s per big cell | separate cluster, quota-capped (D5/D14) |
| Redis geo | 500k members; ~200k writes/s over ~10k H3 keys | 6-node cluster per big cell |
| Realtime connections | ~2M global; ~50k conns/pod ⇒ ~40 pods across cells | D16; connection-count HPA |
| Notifications | ~5k transactional pushes/s peak; campaigns isolated | provider token buckets (D23) |
| Logs | raw ~600k lines/s → <60k/s indexed after sampling | 3 d hot / 30 d object / drop (D27) |
| Recon | 5M ledger match-groups/day; ≥99.5% auto-match ⇒ ≤25k breaks/day ceiling, target <5k | 48 h break SLA (D21) |
| DR overhead | 25–30% standing infra for warm standbys | budgeted in $0.015/order (D25/D27) |
| Verification | every number load-tested at 1.5× monthly in the perf cell; CI diffs model vs results | D24 |

Implementation sequencing, task-level Definition of Done, and test criteria:
see [TASKS.md](../TASKS.md).
