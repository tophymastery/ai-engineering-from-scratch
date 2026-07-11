# Runbook — cart (V-T7)

Owner: **Marketplace** (see `ownership.yaml`). Service: `cart`, port 8104. Flag:
`cart_v1` (ships dark; enable per environment).

## What it does

Owns per-user carts (01 §1) backed by a **Redis snapshot over a durable
PostgreSQL store**. Exposes add/remove/get (via customer-bff) under **ETag/If-Match
optimistic concurrency** (02 §1). Validates + prices line items against the
**merchant-catalog** contract at add time (the `cart → merchant-catalog` pact),
and **revalidates** them by consuming `menu.updated` events (keyed by
`merchant_id`) — a merchant's price change or an item going unavailable is
reflected in affected carts within the freshness window.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Cart ops latency (add/remove/get) | p99 < 100 ms | `CartOpsLatencyHigh` |
| Menu-change revalidation reflected | p99 < 5 s | `CartRevalidationLagHigh` |
| Redis-snapshot hit rate | ≥ 80% | `CartSnapshotHitRateLow` |
| Stale-write protection | 100% of stale writes → 412 | (correctness; tested, not alerted) |

## Key invariants

- **The cart returns a strong `ETag`.** An add/remove on an existing cart
  **requires `If-Match`**; a stale value ⇒ **412 STALE_WRITE**. Under concurrent
  edits exactly one writer wins the version compare-and-swap; every stale writer
  gets 412. 412 is the *correct* rejection of a stale write, **not** data loss.
- **PostgreSQL is the system of record.** The Redis snapshot is a read cache with
  a freshness TTL; on a snapshot miss (eviction / restart / TTL expiry) the cart
  **rehydrates from PG** — the snapshot never masks the durable store.
- **Menu-change reflection ≤ freshness window.** `menu.updated` reprices/flags the
  affected cart lines and eagerly invalidates their snapshots, so the next read
  reflects the change immediately; even if the eager invalidation is missed, the
  snapshot TTL bounds staleness to the window (default 5 s). The consumer dedupes
  by `event_id` (inbox) and orders menus by `version` (LWW) — a duplicate or a
  late/older snapshot never rolls a cart back.

## Common alerts → actions

- **CartOpsLatencyHigh** — check the Redis-snapshot hit rate first
  (`CartSnapshotHitRateLow` often fires alongside): a cold snapshot tier pushes
  reads onto PG. Then check PG CPU / lock waits on the `carts` version CAS. A hot
  cart (many concurrent editors) shows up as elevated 412 too (expected).
- **CartRevalidationLagHigh** — `menu.updated` is taking > 5 s to reflect. Check
  the menu.updated consumer lag / the inbox; carts may show stale prices or stale
  availability at checkout until it drains. Replay is safe (exactly-once + LWW).
- **CartSnapshotHitRateLow** — the snapshot tier is cold or evicting. Check Redis
  health and the TTL config (`CART_SNAPSHOT_TTL`); read latency rises but
  correctness is unaffected (PG rehydrate).
- **CartStaleWriteRateHigh** — a sustained high 412 ratio means clients are
  editing from a stale ETag. Confirm the customer app refreshes the ETag from the
  last write's response (the body + `ETag` header carry the new value). No action
  needed for a transient spike.

## Rollout / rollback

`cart_v1` gates the mutating surface. Enable via `FLAG_CART_V1=true` in the
overlay (staging/preview on; prod via canary). Migrations are expand/contract
(04 §1.3): the additive `0001_cart.pg.sql` ships before the code that uses it.
Rollback = flip the flag off (reads keep working; adds/removes return 404
CART_DISABLED) or roll back the ReplicaSet — both safe.

## Environment adaptations (sandbox)

No Redis daemon → the snapshot tier is an in-process TTL store with the same
fresh/miss contract; no live Kafka → the in-memory eventbus + inbox carry
`menu.updated` (an HTTP inject endpoint `/v1/menu-events` stands in for
cross-process delivery in the shared E2E env); PG is in-memory SQLite in tests
(production schema `services/cart/migrations/0001_cart.pg.sql`). The ETag
concurrency, snapshot/rehydrate, and menu-change revalidation logic are real and
fully tested. See VERIFICATION.md §V-T7.
