-- 0001_pricing.pg.sql — PostgreSQL schema for the V-T8 pricing-promo slice
-- (D10: "Quotes in Redis, HMAC-signed … persisted to PG only at checkout"). The
-- SQLite twin used by tests lives inline in store.go; only the column TYPES
-- differ (TIMESTAMPTZ vs TIMESTAMP, BIGINT vs INTEGER). Additive,
-- expand/contract-only migrations per 04 §1.3.
--
-- CRITICAL PROPERTY (V-T8 #3): the `quotes` table is written ONLY at checkout.
-- POST /v1/quotes writes NOTHING here — the live quote lives in the Redis-like
-- 10-min TTL tier (an in-process quoteCache in this sandbox — see store.go),
-- HMAC-signed so checkout can verify integrity without a read-back. ~99% of
-- quotes are never checked out, so this table sees ~1/50th of pricing's request
-- volume (D10: "pricing's PG write load drops ~50×"). A Redis flush merely forces
-- a re-quote; it never loses a durable row, because durable rows only exist for
-- quotes that reached checkout.

CREATE TABLE IF NOT EXISTS quotes (
    quote_id       TEXT NOT NULL PRIMARY KEY,       -- qot_ prefixed ULID (02 §1)
    cart_id        TEXT NOT NULL,                    -- crt_ prefixed ULID the quote priced
    currency       TEXT NOT NULL,                    -- ISO currency (02 §1 Money)
    subtotal_minor BIGINT NOT NULL,                  -- cart subtotal, integer minor units
    total_minor    BIGINT NOT NULL,                  -- computed total, integer minor units
    fees_json      TEXT NOT NULL DEFAULT '[]',       -- typed fees[] line items (02 §5)
    discounts_json TEXT NOT NULL DEFAULT '[]',       -- typed discounts[] line items (02 §5, negative)
    kid            TEXT NOT NULL,                    -- HMAC signing-key id (rotation)
    signature      TEXT NOT NULL,                    -- base64url HMAC over the canonical quote
    issued_at      TIMESTAMPTZ NOT NULL,             -- quote mint time
    expires_at     TIMESTAMPTZ NOT NULL,             -- issued + 10 min (signed)
    checked_out_at TIMESTAMPTZ NOT NULL DEFAULT now()-- when the quote was consumed at checkout
);

-- Checkout is idempotent on quote_id (INSERT ... ON CONFLICT DO NOTHING): a
-- double "Pay" tap or a BFF retry consuming the same quote never creates a
-- second durable row.
CREATE INDEX IF NOT EXISTS quotes_cart_idx ON quotes (cart_id);
