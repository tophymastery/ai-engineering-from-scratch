-- 0001_payment.pg.sql — V-T10 payment authorize/capture/refund slice
-- (PostgreSQL, production; the Payments team's money-mutation flagship for D9).
--
-- One PG database per service (01 §1). The slice-owned tables live here; the D9
-- idempotency_keys, the D22 outbox, and the consumer inbox are created by their
-- shared-lib migrations (libs/idempotency, libs/outbox, libs/inbox) in the same
-- database. In production `payments`/`payment_events` are partitioned by month +
-- region (01 §5); the sandbox runs the same DDL on in-memory SQLite (types only
-- differ) and the D9 UNIQUE-key-in-tx / exactly-once SEMANTICS are engine-agnostic.
--
-- The money invariant this schema protects (D9): every authorize/capture/refund
-- is a money mutation guarded by UNIQUE(idempotency_key) committed IN THE SAME
-- transaction as the payment row + the payment.* outbox event. A retried money
-- mutation (same key) can never double-charge — the UNIQUE constraint, not the
-- Redis cache, is the source of truth. Redis is a demoted read-through cache.

CREATE TABLE IF NOT EXISTS payments (
    payment_id    TEXT         NOT NULL PRIMARY KEY,   -- pay_ ULID
    order_id      TEXT         NOT NULL,               -- ord_ token (the order aggregate; events key region:order_id)
    customer_id   TEXT         NOT NULL,               -- usr_ token — never PII (D3)
    region        TEXT         NOT NULL,
    method        TEXT         NOT NULL DEFAULT 'card',-- card | wallet
    amount_minor  BIGINT       NOT NULL,               -- integer minor units (02 §1)
    currency      TEXT         NOT NULL,
    status        TEXT         NOT NULL,               -- the payment state machine state
    auth_id       TEXT         NOT NULL DEFAULT '',     -- PSP authorization id
    capture_id    TEXT         NOT NULL DEFAULT '',     -- PSP capture id
    refund_id     TEXT         NOT NULL DEFAULT '',     -- PSP refund id
    psp           TEXT         NOT NULL DEFAULT 'payment-sim',
    webhook_state TEXT         NOT NULL DEFAULT '',     -- last PSP webhook confirmation applied
    created_at    TIMESTAMPTZ  NOT NULL,
    updated_at    TIMESTAMPTZ  NOT NULL
);
-- One live authorization per order (the D9 charge invariant at the schema level):
-- a duplicate authorize for the same order can never create a second charge row.
CREATE UNIQUE INDEX IF NOT EXISTS payments_order_uniq ON payments (order_id);
CREATE INDEX IF NOT EXISTS payments_status_idx ON payments (status, updated_at);
CREATE INDEX IF NOT EXISTS payments_auth_idx ON payments (auth_id);

-- payment_events is the APPEND-ONLY money event store (01 §6). Current status is
-- a pure fold over these rows, so any payment can be replayed for audit. Webhook
-- confirmations are recorded here too (trigger = 'webhook:<type>'), so the
-- "10x webhook replay ⇒ single state transition" property is a row count.
CREATE TABLE IF NOT EXISTS payment_events (
    seq         BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id  TEXT         NOT NULL,
    trigger     TEXT         NOT NULL,
    from_state  TEXT         NOT NULL,
    to_state    TEXT         NOT NULL,
    detail_json JSONB        NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ  NOT NULL
);
CREATE INDEX IF NOT EXISTS payment_events_payment_idx ON payment_events (payment_id, seq);

-- wallets is the stored-value balance per customer (the "+ wallet" scope). A
-- wallet-funded authorize debits the balance inside the SAME D9 money-mutation
-- transaction, so a retried wallet payment debits exactly once.
CREATE TABLE IF NOT EXISTS wallets (
    customer_id   TEXT         NOT NULL PRIMARY KEY,
    region        TEXT         NOT NULL,
    balance_minor BIGINT       NOT NULL DEFAULT 0,
    currency      TEXT         NOT NULL DEFAULT 'THB',
    updated_at    TIMESTAMPTZ  NOT NULL
);

-- wallet_entries is the append-only wallet ledger (debits negative, credits
-- positive) — the audit trail behind every balance change.
CREATE TABLE IF NOT EXISTS wallet_entries (
    entry_id    TEXT         NOT NULL PRIMARY KEY,    -- wal_ ULID
    customer_id TEXT         NOT NULL,
    payment_id  TEXT         NOT NULL DEFAULT '',
    delta_minor BIGINT       NOT NULL,
    reason      TEXT         NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL
);
CREATE INDEX IF NOT EXISTS wallet_entries_customer_idx ON wallet_entries (customer_id, created_at);
