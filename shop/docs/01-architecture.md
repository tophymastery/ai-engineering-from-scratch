# 01 — Architecture: Microservices & BFF

Rust services on Kubernetes (async on tokio; axum for HTTP, tonic for gRPC,
sqlx for PostgreSQL, rdkafka for Kafka), one bounded context and one PostgreSQL
database per service, Kafka as the async backbone, BFFs shaping payloads per
client. Backend language is Rust end-to-end per decision D31 (doc 05 §3.J).
Sync calls (gRPC/HTTP) only for request/response reads and commands; every
state change that other services care about is published as an event.

## 1. Service catalog

| Service | Responsibility | Owns (DB) | Publishes | Consumes | Sync API (key ops) |
|---|---|---|---|---|---|
| `identity` | Accounts, sessions, roles (customer/merchant/driver/ops), device tokens | users, sessions, roles | `user.registered` | — | register, login, introspect token |
| `merchant-catalog` | Merchants, stores, menus, items, availability, opening hours | merchants, menus, items | `menu.updated`, `store.status_changed` | — | CRUD menus, set availability |
| `search` | Store/dish discovery, ranking, geo search | search index (read model) | — | `menu.updated`, `store.status_changed`, `rating.updated` | search stores/dishes near point |
| `cart` | Per-user carts, item validation against catalog | carts | — | `menu.updated` (revalidate) | add/remove item, get cart |
| `order` | **Saga orchestrator**: order lifecycle, state machine, compensation | orders, order_events (event store), outbox | `order.*` (created, paid, accepted, dispatched, picked_up, delivered, cancelled) | `payment.*`, `dispatch.*` | checkout, cancel, get order |
| `payment` | Authorize/capture/refund via PSP adapters; wallet | payments, ledger, outbox | `payment.authorized`, `payment.captured`, `payment.failed`, `payment.refunded` | `order.created`, `order.cancelled` | authorize, capture, refund |
| `pricing-promo` | Quotes: item total, delivery fee, surge, promos, vouchers | promo rules, vouchers, quotes | `promo.redeemed` | `order.created` | quote(cart, location, time) |
| `dispatch` | Driver matching, offers, assignment, re-dispatch | assignments, offers, outbox | `dispatch.offered`, `dispatch.assigned`, `dispatch.failed` | `order.paid`, driver location snapshots | accept/decline offer |
| `location-tracking` | Live driver GPS ingestion, ETA, geofencing | recent tracks (hot in Redis geo, cold in PG) | `driver.location_updated` (sampled) | — | driver position stream, order ETA |
| `notification` | Push/SMS/email fan-out, templates, per-user preferences | templates, deliveries | — | `order.*`, `dispatch.*`, `payment.*` | — (consumer only) |
| `rating` | Ratings & reviews for merchants/drivers/orders | ratings | `rating.updated` | `order.delivered` (unlock rating) | rate order |
| `settlement` | Merchant/driver payouts, commission, reconciliation | settlements, payout runs | `settlement.completed` | `order.delivered`, `payment.captured`, `payment.refunded` | payout reports |

Rules that keep this catalog coherent:
- **A service is the only writer of its tables.** Cross-service reads happen via
  its API or by consuming its events into a local read model (as `search` does).
- **No distributed transactions.** Multi-service consistency is the order saga.
- New capability → new service **only** when it has its own data + lifecycle;
  otherwise it's a module inside an existing bounded context.

## 2. BFF layer

One BFF per client, Rust/axum (aggregation only, no business logic), deployed
like any other service:

| BFF | Client | Typical aggregation |
|---|---|---|
| `customer-bff` | Customer iOS/Android/web | home feed = `search` + `pricing-promo` (fees) + `rating`; order detail = `order` + `location-tracking` (live ETA) + `payment` |
| `merchant-bff` | Merchant tablet/app | incoming orders queue = `order` + `pricing-promo` breakdown; menu editor = `merchant-catalog` |
| `driver-bff` | Driver app | offer card = `dispatch` + `order` summary + `location-tracking` route; earnings = `settlement` |
| `admin-bff` | Ops web console | cross-service lookups, refund console, merchant onboarding |

BFF contract:
- **Aggregation, translation, and client-shaped payloads only. No business
  logic, no data ownership, no direct DB access.** If a BFF needs a rule, that
  rule belongs in a service.
- Talks to services via gRPC with per-call deadlines and circuit breakers;
  degrades gracefully (partial responses with `warnings[]`) when a non-critical
  upstream is down.
- The **API Gateway** in front of all BFFs does authn (token introspection via
  `identity`), coarse rate limiting, TLS termination, and mints the edge
  `request_id` (doc 04 §3).

## 3. Event backbone: Kafka + outbox + registry

- **Topics** are `<domain>.<event>` (e.g. `order.paid`), keyed by
  `region:aggregate_id` so one aggregate's events stay ordered in one partition
  while regions scale independently.
- **Outbox pattern** everywhere a DB write must produce an event: the service
  writes the row *and* an `outbox` row in the same local transaction; a relay
  (Debezium or an in-service poller) publishes and marks it sent. No
  write-then-publish races, no lost events.
- **Consumer inbox**: every consumer records processed `event_id`s in an
  `inbox` table (same transaction as its side effects) and skips duplicates —
  at-least-once delivery, exactly-once *effect*.
- **Schema registry** with backward-compatible evolution only (add optional
  fields; never rename/repurpose). Event contracts live in doc 02 §4.

## 4. Order saga

`order` is the orchestrator; steps are commands to other services, each with a
compensation:

| Step | Command | On failure (compensation) |
|---|---|---|
| 1 | `pricing-promo.quote` | reject checkout (no side effects yet) |
| 2 | `payment.authorize` | cancel order |
| 3 | merchant accept (via `merchant-bff` → `order`) | `payment.refund` (void auth), cancel |
| 4 | `dispatch.assign` | retry ×N with widening radius → refund + cancel |
| 5 | pickup → delivery (driver events) | re-dispatch on driver abandon |
| 6 | `payment.capture` + `settlement` accrual | ops alert, manual reconciliation queue |

Timeouts drive the saga forward too: merchant not accepting in `T_accept` or no
driver in `T_dispatch` triggers the compensation path automatically.

### Order state machine

Explicit transition table — anything not listed is rejected with `409 INVALID_TRANSITION`:

```
CREATED        -> QUOTED            (quote ok)
QUOTED         -> PAYMENT_PENDING   (checkout confirmed)
PAYMENT_PENDING-> PAID              (payment.authorized)
PAYMENT_PENDING-> CANCELLED         (payment.failed | user cancel | timeout)
PAID           -> ACCEPTED          (merchant accept)
PAID           -> CANCELLED         (merchant reject | T_accept timeout)   [refund]
ACCEPTED       -> DISPATCHED        (dispatch.assigned)
ACCEPTED       -> CANCELLED         (T_dispatch exhausted)                 [refund]
DISPATCHED     -> PICKED_UP         (driver pickup scan)
DISPATCHED     -> ACCEPTED          (driver abandon -> re-dispatch)
PICKED_UP      -> DELIVERED         (driver delivery confirm/geofence)
DELIVERED      -> SETTLED           (capture + settlement accrual)
```

## 5. Scalability

| Concern | Mechanism |
|---|---|
| Compute | Stateless services; K8s HPA on CPU + custom metrics (Kafka consumer lag, RPS); PodDisruptionBudgets |
| Traffic shape | Partition by geo: region is part of routing keys, Kafka keys, and cache keys; a city's incident stays in that city |
| Database | One PG per service; read replicas for read-heavy paths (catalog, search-source); PgBouncer; partition `orders`/`order_events` by month + region |
| Hot paths | Redis: driver live locations (GEO sets, TTL), session/token cache, idempotency keys, cart snapshots |
| Search | Dedicated index (OpenSearch) fed by events — reads never hit catalog PG |
| Spikes | Kafka absorbs bursts (dispatch, notifications are async); BFF response caching for feed/browse (short TTL, per-geo key) |
| Budgets | p99 targets: BFF read 300 ms, checkout 800 ms, dispatch assignment 5 s; each service publishes its own p99 budget and is alerted on burn (doc 04 §2) |
| Capacity anchor | Design point: 1M orders/day, ×10 peak-hour skew ≈ 1.2k orders/min sustained; location ingest 50k msg/s sampled to 5k/s published |

## 6. Determinism

Same inputs ⇒ same outputs, everywhere it matters:

- **State machine over ad-hoc ifs** — the transition table above is data; the
  engine rejects everything else. No hidden state.
- **Injected time and randomness.** No service calls `time.Now()` or
  `rand.*` in business logic; a `Clock` and `Rand` interface is injected
  (production impls wrap the real ones). Tests freeze the clock and seed the
  RNG ⇒ byte-identical reruns (doc 03 §4).
- **Event-sourced order history**: `order_events` is append-only; current state
  is a pure fold over events, so any order can be replayed for audit, debugging,
  or migration verification.
- **Deterministic dispatch scoring**: driver matching = pure function
  `score(driver, order, now)` over a snapshot of candidates; the snapshot
  (inputs) is logged with the decision, so every assignment is explainable and
  reproducible.
- **Idempotent consumers + ordered partitions** (§3) make event replay safe:
  reprocessing a partition from any offset converges to the same state.
