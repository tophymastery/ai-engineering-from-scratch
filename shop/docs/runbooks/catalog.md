# Runbook — merchant-catalog (V-T3)

Owner: **Discovery** (see `ownership.yaml`). Service: `merchant-catalog`, port
8102. Flag: `catalog_v1` (ships dark; enable per environment).

## What it does

Owns merchants, menus, items, availability and store status (01 §1). Exposes the
menu editor + store-status endpoints (via merchant-bff) under **ETag/If-Match
optimistic concurrency** (02 §1), and publishes `menu.updated` +
`store.status_changed` (keyed by `merchant_id`) through the **transactional
outbox** to `search` + `cart`.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Menu CRUD latency | p99 < 200 ms | `CatalogMenuCRUDLatencyHigh` |
| Event publish lag | p99 < 2 s | `CatalogEventPublishLagHigh` |
| Stale-write protection | 100% of stale writes → 412 | (correctness; tested, not alerted) |

## Key invariants

- **Every mutable resource returns a strong `ETag`.** A `PATCH /menu` / `PUT
  /store-status` **requires `If-Match`**; a stale value ⇒ **412 STALE_WRITE**.
  412 is the *correct* rejection of a stale write, **not** data loss.
- **Exactly-once publish.** The DB write and the outbox row are one transaction,
  so a rejected (412) edit publishes **nothing**, and an accepted edit publishes
  **exactly one** event. Consumers dedupe by `event_id` (inbox).

## Common alerts → actions

- **CatalogMenuCRUDLatencyHigh** — check DB CPU / lock waits on the `menus` /
  `store_status` version CAS. A hot merchant (many concurrent editors) shows up
  as elevated 412 too (expected). Scale read replicas for the GET path.
- **CatalogEventPublishLagHigh / CatalogOutboxBacklogGrowing** — the CDC relay is
  behind. Check Kafka reachability and the relay pod; the outbox is durable so no
  events are lost, but `search`/`cart` are stale until it drains. Replay is safe
  (idempotent consumers).
- **CatalogStaleWriteRateHigh** — a sustained high 412 ratio means clients are
  editing from a stale ETag. Confirm the merchant app refreshes the ETag from the
  last write's response (the response body + `ETag` header carry the new value).
  No action needed for a transient spike during a bulk edit.

## Rollout / rollback

`catalog_v1` gates the mutating surface. Enable via `FLAG_CATALOG_V1=true` in the
overlay (staging/preview on; prod via canary). Migrations are expand/contract
(04 §1.3): the additive `0001_catalog.pg.sql` ships before the code that uses it.
Rollback = flip the flag off (reads keep working; edits return 404
CATALOG_DISABLED) or roll back the ReplicaSet — both safe.
