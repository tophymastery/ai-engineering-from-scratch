# Runbook — order (V-T9)

Team: **Marketplace** · Slot port **8105** · Flag **`saga_v1`** · Decisions **D22** (CDC
outbox/inbox exactly-once, DLQ+replay) + **D9** (transaction-durable idempotency).

## What it does

`order` is the **saga orchestrator** for the order lifecycle (docs/01 §4). It owns
the explicit **state machine**, drives the saga against the payment/dispatch/
pricing contracts + fakes, and is the flagship consumer of both the **idempotency
lib** (idempotent checkout) and the **outbox+inbox CDC path** (produces `order.*`,
consumes `payment.*`/`dispatch.*`/`driver.*` exactly-once). It runs a **durable
timer table + leased sweeper** for `T_accept` / `T_dispatch` / capture-by and the
**PAYMENT_PENDING remediation** timer, and exposes **bulk-compensation** +
**stuck-order console** admin endpoints.

Endpoints: `POST /v1/orders` (checkout, idempotent, `saga_v1`-gated),
`GET /v1/orders/{id}`, `POST /v1/orders/{id}:cancel|:accept|:reject`,
`POST /v1/order-events` (inbound domain-event delivery),
`POST /v1/admin/orders:bulk-cancel`, `GET /v1/admin/orders/stuck`,
`POST /v1/admin/sweep`.

## The state machine (docs/01 §4 — authoritative)

```
CREATED -> QUOTED -> PAYMENT_PENDING -> PAID -> ACCEPTED -> DISPATCHED -> PICKED_UP -> DELIVERED -> SETTLED
PAYMENT_PENDING -> CANCELLED (payment.failed | user cancel | timeout[void])
PAID            -> CANCELLED (merchant reject | T_accept timeout)   [refund]
ACCEPTED        -> CANCELLED (T_dispatch exhausted)                 [refund]
DISPATCHED      -> ACCEPTED  (driver abandon -> re-dispatch)
```
Anything not in the table ⇒ **409 ORDER_INVALID_TRANSITION**. Current state is a
pure fold over the append-only `order_events` store (replayable).

## SLOs

- **Stuck-order ratio < 0.05%/day** — non-terminal orders wedged past their timer
  windows / total daily orders. The headline saga-health SLO.
- **Durable timers fire within 60s of due** — the crash-survival guarantee.
- **Auto-remediation < 16 min** — a stuck PAYMENT_PENDING is voided+cancelled by
  the remediation timer (armed at checkout+15m).
- **Zero duplicate charges, zero lost orders** — the money-path invariant.
- **Checkout p99 < 800 ms** (01 §5).

## Key invariants

1. **Every state change goes through the state machine.** Illegal transition ⇒
   409, no mutation. No ad-hoc `if`s.
2. **Effect-once checkout (D9).** The order row + `order.created` event + the
   remediation timer commit atomically with the `UNIQUE(idempotency_key)` insert.
   A double "Pay" tap + BFF retry ⇒ **one** order, **one** authorization.
3. **Exactly-once consumption (D22).** `payment.*`/`dispatch.*`/`driver.*` are
   deduped by `event_id` in the durable inbox; a Kafka redelivery ⇒ one effect.
4. **Durable timers, leased.** Timers live in the `timers` table, not memory. A
   crash loses only the sweeper goroutine; a restart reclaims every due row. The
   `PENDING→FIRING` claim (guarded UPDATE / `FOR UPDATE SKIP LOCKED`) fires each
   timer exactly once even with N sweepers.
5. **Compensation is idempotent + post-commit.** void/refund/capture run once per
   transition; a crash before the side-effect is caught by the timers (a stuck
   PAYMENT_PENDING is voided+cancelled by remediation).

## Common alerts → actions

- **OrderStuckRatioHigh** (crit): saga wedged. Check the sweeper is firing
  (`order_timer_fire_lag`), the payment/dispatch consumers, and the inbox DLQ.
  Triage with `GET /v1/admin/orders/stuck`; remediate a cohort with
  `POST /v1/admin/orders:bulk-cancel`.
- **OrderTimerFireLagHigh** (crit): due timers not firing < 60s. Check sweeper
  liveness, lease contention, DB latency on `timers`. `POST /v1/admin/sweep`
  fires due timers on demand.
- **OrderRemediationBacklog** (warn): remediation timers pending but not firing —
  stuck PAYMENT_PENDING may miss the 16-min void. Check the sweeper.
- **OrderDuplicateChargeDetected** (crit): the exactly-once path leaked. Freeze
  the key range, inspect `idempotency_keys` + `inbox`, page Payments.
- **OrderInboxDLQDepthHigh** (warn): saga events parked. Inspect + replay with
  `tools/dlqctl`.
- **OrderCheckoutLatencyHigh** (warn): checkout p99 > 800ms. Check the idempotency
  DB path + outbox insert + payment authorize round-trip.

## Rollout / rollback

`saga_v1` ships **dark** (prod overlay `FLAG_SAGA_V1=false`); staging/preview and
the E2E realcmd force it **on**. Enable in prod via a canary-gated rollout
(Argo Rollouts). Rollback = flip the flag off (checkout ⇒ 404 SAGA_DISABLED); the
durable timers + inbox keep resolving in-flight orders. Per-request
`X-Flag-Override` honoured only in non-prod.

## Environment adaptations (sandbox)

- **No Docker/K8s** ⇒ process-mode; manifests render-only (`make render-order`).
  "Kill all order pods" ⇒ **discard the in-memory sweeper, retain the durable
  timers table, restart** — the durable-fire property is FULL; the pod-kill is
  the adaptation.
- **No live Kafka** ⇒ in-memory eventbus + the **durable SQL inbox** (the
  exactly-once path is real); `tools/dlqctl` replay exercised.
- **No PG** ⇒ in-memory SQLite in tests; production schema
  `services/order/migrations/0001_order.pg.sql`. The state machine, 1000/1000
  durable-timer fire, exactly-one-effect, and remediation-once are genuine and
  run under `-race`; only wall-clock durations are compressed to a frozen clock.
