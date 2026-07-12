-- 0001_order.pg.sql — V-T9 order saga slice (PostgreSQL, production).
--
-- One PG database per service (01 §1). Three slice-owned tables live here; the
-- D9 idempotency_keys, the D22 outbox, and the consumer inbox are created by
-- their shared-lib migrations (libs/idempotency, libs/outbox, libs/inbox) in the
-- same database. In production `orders`/`order_events` are partitioned by month +
-- region (01 §5); the sandbox runs the same DDL on in-memory SQLite (types only
-- differ), and the durable-timer / event-store / exactly-once SEMANTICS are
-- engine-agnostic.

CREATE TABLE IF NOT EXISTS orders (
    order_id     TEXT         NOT NULL PRIMARY KEY,   -- ord_ ULID
    customer_id  TEXT         NOT NULL,               -- usr_ token — never PII (D3)
    merchant_id  TEXT         NOT NULL,
    quote_id     TEXT         NOT NULL,               -- the pricing-promo quote consumed at checkout
    region       TEXT         NOT NULL,
    status       TEXT         NOT NULL,               -- the 01 §4 state machine state
    total_minor  BIGINT       NOT NULL,               -- integer minor units (02 §1)
    currency     TEXT         NOT NULL,
    auth_id      TEXT         NOT NULL DEFAULT '',     -- payment authorization hold
    capture_id   TEXT         NOT NULL DEFAULT '',     -- capture at settlement
    created_at   TIMESTAMPTZ  NOT NULL,
    updated_at   TIMESTAMPTZ  NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_status_idx ON orders (status, updated_at);

-- order_events is the APPEND-ONLY event store (01 §6). Current state is a pure
-- fold over these rows, so any order can be replayed for audit / migration.
CREATE TABLE IF NOT EXISTS order_events (
    seq         BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id    TEXT         NOT NULL,
    trigger     TEXT         NOT NULL,
    from_state  TEXT         NOT NULL,
    to_state    TEXT         NOT NULL,
    detail_json JSONB        NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ  NOT NULL
);
CREATE INDEX IF NOT EXISTS order_events_order_idx ON order_events (order_id, seq);

-- timers is the DURABLE timer table (T_accept / T_dispatch / capture-by / the
-- PAYMENT_PENDING remediation timer). A leased sweeper fires each due timer
-- exactly once: PENDING → FIRING (claim under lease) → FIRED. Surviving a process
-- crash ⇒ the table + the lease, not in-memory state, are the source of truth.
-- The leased-claim maps to `... FOR UPDATE SKIP LOCKED` on PostgreSQL.
CREATE TABLE IF NOT EXISTS timers (
    timer_id     TEXT         NOT NULL PRIMARY KEY,   -- tmr_ ULID
    order_id     TEXT         NOT NULL,
    kind         TEXT         NOT NULL,               -- payment_remediation | t_accept | t_dispatch | capture_by
    trigger      TEXT         NOT NULL,               -- the saga trigger to fire when due
    due_at       TIMESTAMPTZ  NOT NULL,
    status       TEXT         NOT NULL DEFAULT 'PENDING', -- PENDING | FIRING | FIRED | CANCELLED
    leased_by    TEXT         NOT NULL DEFAULT '',
    leased_until TIMESTAMPTZ,
    fired_at     TIMESTAMPTZ,
    created_at   TIMESTAMPTZ  NOT NULL
);
CREATE INDEX IF NOT EXISTS timers_due_idx ON timers (status, due_at);
CREATE INDEX IF NOT EXISTS timers_order_idx ON timers (order_id);
