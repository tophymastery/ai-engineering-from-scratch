# Runbook — dispatch (V-T12, D13 zone-owned batch matching)

**Team:** Logistics · **Slot:** port 8108 · **Flag:** `dispatch_batch`

Dispatch owns driver assignment: paid orders become waiting orders in their H3
zone; a per-zone single-writer tick batch-matches them to available drivers
(greedy-with-swaps); each matched driver gets an **exclusive 10 s reservation**
before the offer (replacing first-accept-wins 409s); the driver's accept assigns
the order. Every batch logs a **deterministic snapshot** so assignments replay
byte-identically and are explainable.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Assignment latency p95 (order-ready → assigned) | **< 5 s** (tick ≤2 s + offer ≤3 s) | `DispatchAssignmentLatencyHigh` |
| Offer-conflict rate | **< 0.5%** | `DispatchOfferConflictRateHigh` |
| Reservation-leak rate | **0** | `DispatchReservationLeak` |
| Snapshot replay | **100% identical** | `DispatchSnapshotReplayMismatch` |
| Batch quality vs greedy | **≥10% lower sum-of-pickup-ETA** | dashboard panel |

## Key invariants

1. **Zone single-writer.** Each H3 res-5 zone pins to one Kafka partition (D13),
   so one consumer/writer owns the zone per tick — no two ticks assign the same
   driver. The engine holds a per-zone lock across a whole tick.
2. **Exclusive reservations, no 409.** A driver is reserved exclusively (10 s TTL)
   before the offer; a second batch cannot offer that driver. There is NO
   first-accept-wins 409 path — the reservation prevents the conflict.
3. **Zero reservation leak.** Every reservation is consumed by an accept or
   released on expiry (the sweeper). Ledger accounting: `created == consumed +
   released + held_live`, so `leaked == 0` at all times.
4. **Deterministic replay.** Each tick logs its full inputs + RNG seed; the matcher
   runs on an injected clock + seeded RNG + a pure ETA source, so replaying a
   snapshot reproduces byte-identical assignments.

## Common alerts → actions

- **DispatchAssignmentLatencyHigh** — check per-zone tick duration (should be
  1–2 s; a hot zone with thousands of orders may need a finer partition split),
  the offer→accept round-trip, driver supply density, and the map-sim ETA source
  latency. Inspect `GET /v1/admin/snapshots` for oversized batches.
- **DispatchOfferConflictRateHigh** — two batches are contending for a driver.
  Check zone→partition assignment (single-writer-per-zone) and any recent
  partition rebalance; verify the reservation TTL is sane. `GET
  /v1/admin/reservations` shows the live conflict rate.
- **DispatchReservationLeak** — the sweeper is not reclaiming expired holds.
  Inspect the ledger via `GET /v1/admin/reservations` (`leaked > 0`), confirm the
  tick loop is running, and check the injected clock.
- **DispatchSnapshotReplayMismatch** — nondeterminism leaked into the matcher
  (an unseeded clock/RNG or an impure ETA source). Freeze deploys; replay the
  offending snapshot with `GET /v1/admin/snapshots/{tick_id}` (`replay_identical`
  will be false) and bisect.
- **DispatchInboxDLQDepthHigh** — order.paid / driver.location_updated events
  parked; the matcher is missing orders or driver locations. Inspect + replay with
  `tools/dlqctl`.

## Queryable snapshot log

- `GET /v1/admin/snapshots?limit=N` — recent batch snapshots (zone, seed,
  n_orders/n_drivers/n_assigned, replay_ok).
- `GET /v1/admin/snapshots/{tick_id}` — one snapshot + on-demand replay
  verification (`replay_identical`).
- `GET /v1/admin/reservations` — reservation ledger stats (conflict rate, leak).

## Sandbox adaptations (disclosed in VERIFICATION §V-T12)

No Docker/K8s ⇒ process-mode + render-only manifests; no live Kafka ⇒ in-memory
eventbus + durable SQL inbox (partition-per-zone in code + config, the
single-writer invariant is real); no PG ⇒ in-memory SQLite in tests (production
schema `services/dispatch/migrations/0001_dispatch.pg.sql`); map-sim ETAs use the
deterministic in-process twin for byte-identical replay. The 24 h leak soak and
1.5× density are frozen-clock/adapted; the zero-leak, <0.5%-conflict, 100%-replay,
and ≥10%-ETA invariants are FULL.
