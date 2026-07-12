# Runbook — merchant-queue (V-T11)

Team: **Marketplace** · Slot port **8117** · Flag **`merchant_queue_v1`** ·
Decisions **D7** (CQRS incoming-order read model + kitchen-capacity admission) +
**D11** (sharding by `merchant_id`).

## What it does

`merchant-queue` owns the **merchant incoming-order queue** — a CQRS read model
**projected exactly-once from `order.*` events** (via the partitioned inbox,
S-T6), **sharded by `merchant_id`** (libs/sharding, D11). It exposes the
merchant-bff accept/reject surface: an admitted accept **drives the order saga**
(`POST /v1/orders/{id}:accept` → `order.accepted`). Accepts are metered by a
**kitchen-capacity admission control** (default **30 accepts / 10 min**,
merchant-tunable): when the kitchen is at capacity the accept is **deferred with a
busy badge + inflated prep ETA** — it **never fails checkout**. A **rebuild** tool
reconstructs the read model (or one cell) from the append-only event log.

Endpoints: `GET /v1/merchant/orders?merchant_id=…&state=` (the queue),
`GET /v1/merchant/orders/{id}`, `POST /v1/merchant/orders/{id}:accept|:reject`,
`GET|PUT /v1/merchant/{merchant_id}/capacity` (busy badge + tuning),
`POST /v1/order-events` (inbound order.* delivery),
`POST /v1/admin/rebuild[?cell=N]`, `GET /v1/admin/freshness`.

## Projection (D7) — the read model is a fold over the event log

Every projected `order.*` event is applied to `incoming_orders` **and** appended
to `order_event_log` on the **same inbox transaction** — so the read model and the
log are always consistent, and a redelivered `event_id` is a no-op (exactly-once).
Ordering is **LWW forward-only** by a monotonic lifecycle `phase`
(created<paid<accepted<…; cancelled is terminal), so out-of-order delivery across
the salted merchant partitions converges. `order.paid` is the event that puts an
order **into the accept queue** (state `PENDING`) and is the **freshness datum**.

Rebuild: `POST /v1/admin/rebuild` (whole store) or `?cell=N` (one physical cell —
the "rebuild the largest cell" drill). It drops the target rows, replays the log
in `seq` order through the same fold, and **asserts parity** with the pre-rebuild
model (`parity_ok`, `mismatches`).

## Admission (D7) — kitchen capacity, busy badge, NOT checkout failure

A per-merchant **sliding-window token bucket** (`accepts_per_window` /
`window_seconds`, default 30/600). `TryAccept` grants a token iff fewer than
`capacity` grants fall inside the window; otherwise the accept is **deferred**
(HTTP 200, `busy:true`, inflated `prep_eta_minutes`). The token is **refunded** if
the downstream saga accept did not actually apply, so the admitted rate tracks
real accepts (**accept rate = configured capacity ± 5%**).

## SLOs & alerts (deploy/alerts/merchant-queue.yaml)

| Alert | Condition | Action |
|---|---|---|
| `MerchantQueueFreshnessLagHigh` | order.paid→visible p99 > 2s | check inbox consumer lag + DLQ + per-cell workers |
| `MerchantQueueProjectionParityDrift` | rebuild parity mismatches > 0 | freeze the cell, `POST /v1/admin/rebuild?cell=N`, inspect inbox |
| `MerchantQueueAcceptRateOffCapacity` | admitted rate off capacity > 5% | check the token ledger (Redis) + capacity config |
| `MerchantQueueCheckout5xx` | any checkout 5xx on the queue path | page Marketplace — capacity pressure must degrade to a busy badge, never 5xx |
| `MerchantQueueInboxDLQDepthHigh` | projection DLQ non-empty | inspect + replay with `tools/dlqctl`, then rebuild |

## Rebuild drill (D7 Tier-1 rebuild-from-events)

```
make rebuild-merchant-queue     # seeds N orders, rebuilds the largest cell from the log, asserts 100% parity, prints wall time
# or against a live slot:
curl -X POST "$GW/merchant-queue/v1/admin/rebuild?cell=2"   # {parity_ok:true, mismatches:0}
```

The read model is Tier-1 (rebuildable) — on a cell evacuation (V-T34) it is rebuilt
from the replicated `order.*` events, targeting < 1h for the largest cell with
100% projection parity.

## Sandbox adaptations (disclosed)

No Docker/K8s ⇒ process-mode + render-only manifests. No live Kafka ⇒ in-memory
eventbus + **durable SQL inbox** (the exactly-once projection path is real). No PG
⇒ in-memory SQLite in tests (production schema
`services/merchant-queue/migrations/0001_merchant_queue.pg.sql`). The admission
token ledger is in-process here (Redis per-cell in production). The projection,
LWW, admission arithmetic, and rebuild correctness are real.
