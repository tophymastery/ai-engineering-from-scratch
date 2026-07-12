-- 0001_dispatch.pg.sql — production PostgreSQL schema for the V-T12 Dispatch &
-- driver-offer slice (Logistics team; D13 zone-owned batch matching). One PG
-- database per service (01 §1). The runtime in this sandbox uses in-memory SQLite
-- (process-mode; no PG daemon — disclosed in VERIFICATION §V-T12); the
-- snapshot-log + assignment SEMANTICS are engine-agnostic and identical on either
-- (types only differ: TIMESTAMPTZ vs TIMESTAMP, BIGINT vs INTEGER).
--
-- dispatch_snapshots is the DETERMINISTIC, EXPLAINABLE batch log (D13: "Each batch
-- logs its full input snapshot ⇒ deterministic and explainable"). Every zone tick
-- records its full inputs (waiting orders, available drivers), its RNG seed, and
-- the assignments it produced, so any assignment is auditable and the batch can be
-- REPLAYED to reproduce byte-identical assignments (correctness property #1). The
-- replay_ok column is the durable evidence that replay reproduced the logged
-- assignments at persist time.

CREATE TABLE IF NOT EXISTS dispatch_snapshots (
    tick_id     BIGINT NOT NULL PRIMARY KEY,   -- monotonic per-cell tick id (also the RNG seed input)
    zone_key    TEXT NOT NULL,                 -- H3 res-5 zone key (single-writer ownership unit)
    partition   INTEGER NOT NULL,              -- Kafka partition the zone pins to (D13 partition-per-zone)
    at          TIMESTAMPTZ,                   -- tick time (injected clock)
    seed        BIGINT NOT NULL,               -- deterministic RNG seed for this tick
    n_orders    INTEGER NOT NULL DEFAULT 0,
    n_drivers   INTEGER NOT NULL DEFAULT 0,
    n_assigned  INTEGER NOT NULL DEFAULT 0,
    replay_ok   INTEGER NOT NULL DEFAULT 0,    -- 1 iff replay reproduced identical assignments
    orders      TEXT NOT NULL DEFAULT '[]',    -- JSON: the waiting orders (matcher input)
    drivers     TEXT NOT NULL DEFAULT '[]',    -- JSON: the available drivers (matcher input)
    assignments TEXT NOT NULL DEFAULT '[]'     -- JSON: the produced assignments (matcher output)
);
CREATE INDEX IF NOT EXISTS dispatch_snapshots_zone_idx ON dispatch_snapshots (zone_key, tick_id);

-- assignments is the assignment read model: the current (order → driver) mapping
-- and its lifecycle status. PENDING (offered, reservation held) → ASSIGNED (driver
-- accepted) → terminal. Keyed by order_id.
CREATE TABLE IF NOT EXISTS assignments (
    order_id      TEXT NOT NULL PRIMARY KEY,
    driver_id     TEXT NOT NULL DEFAULT '',
    assignment_id TEXT NOT NULL DEFAULT '',
    pickup_eta_s  INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'PENDING',  -- PENDING|ASSIGNED|FAILED
    zone_key      TEXT NOT NULL DEFAULT '',
    assigned_at   TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL
);
