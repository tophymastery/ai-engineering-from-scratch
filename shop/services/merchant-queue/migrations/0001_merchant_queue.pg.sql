-- 0001_merchant_queue.pg.sql — production PostgreSQL schema for the V-T11
-- Merchant accept & order-queue slice (Marketplace team; D7 CQRS read model +
-- D11 sharding by merchant_id). One PG database per service (01 §1). The runtime
-- in this sandbox uses in-memory SQLite (process-mode; no PG daemon — disclosed
-- in VERIFICATION §V-T11); the projection / LWW / rebuild SEMANTICS are
-- engine-agnostic and identical on either (types only differ: TIMESTAMPTZ vs
-- TIMESTAMP, BIGINT vs INTEGER).
--
-- The read model is a PURE FOLD over order_event_log (D7 Tier-1
-- rebuild-from-events): drop incoming_orders, replay the log in seq order, and
-- the model reconstructs byte-for-byte. Sharded by merchant_id: each row carries
-- its logical shard (libs/sharding.LogicalShard(merchant_id), 256 shards) and its
-- physical cell (shard -> cell map, D11) so a rebuild can target one cell.

-- incoming_orders: the CQRS read model — the merchant incoming-order queue.
-- Projected exactly-once from order.* events via the partitioned inbox (S-T6).
CREATE TABLE IF NOT EXISTS incoming_orders (
    order_id      TEXT NOT NULL PRIMARY KEY,
    merchant_id   TEXT NOT NULL DEFAULT '',
    shard         INTEGER NOT NULL DEFAULT -1,      -- logical shard (D11): LogicalShard(merchant_id)
    cell          INTEGER NOT NULL DEFAULT -1,      -- physical cell: shard -> cell map
    customer_id   TEXT NOT NULL DEFAULT '',
    total_minor   BIGINT NOT NULL DEFAULT 0,
    currency      TEXT NOT NULL DEFAULT '',
    queue_state   TEXT NOT NULL DEFAULT 'CREATED',  -- CREATED|PENDING|ACCEPTED|REJECTED|CANCELLED|DISPATCHED|PICKED_UP|DELIVERED|SETTLED
    phase         INTEGER NOT NULL DEFAULT 0,       -- monotonic lifecycle rank (LWW ordering)
    created_at    TIMESTAMPTZ,
    paid_at       TIMESTAMPTZ,                      -- order.paid occurred_at (freshness datum)
    accepted_at   TIMESTAMPTZ,
    last_event_at TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS incoming_orders_merchant_idx ON incoming_orders (merchant_id, queue_state);
CREATE INDEX IF NOT EXISTS incoming_orders_cell_idx ON incoming_orders (cell);

-- order_event_log: append-only log of every projected order.* event — the
-- rebuild source of truth. Written on the SAME inbox transaction as the read
-- model apply, so the log and the model are always consistent (a crash can never
-- leave a projected effect without its log row). Partition by day in production
-- (mirrors the outbox/inbox partitioning); dropped-partition cleanup is the same
-- runbook as S-T6.
CREATE TABLE IF NOT EXISTS order_event_log (
    seq         BIGSERIAL PRIMARY KEY,
    event_id    TEXT NOT NULL,
    order_id    TEXT NOT NULL,
    merchant_id TEXT NOT NULL DEFAULT '',
    event_type  TEXT NOT NULL,
    phase       INTEGER NOT NULL,
    occurred_at TIMESTAMPTZ,
    payload     TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS order_event_log_order_idx ON order_event_log (order_id, seq);

-- kitchen_capacity: per-merchant admission configuration (D7 kitchen-capacity
-- admission tokens). Default 30 accepts / 600 s, merchant-tunable. The live token
-- ledger (sliding-window grant timestamps) is held in the admission controller
-- (Redis in production; in-process here — disclosed); this table is the durable
-- CONFIG the controller loads.
CREATE TABLE IF NOT EXISTS kitchen_capacity (
    merchant_id       TEXT NOT NULL PRIMARY KEY,
    accepts_per_window INTEGER NOT NULL DEFAULT 30,
    window_seconds    INTEGER NOT NULL DEFAULT 600,
    updated_at        TIMESTAMPTZ NOT NULL
);
