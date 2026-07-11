-- 0001_idempotency.pg.sql — D9 durable idempotency table (PostgreSQL, production).
--
-- Adopting slices run this via idempotency.Migrate() or their own migration
-- runner (expand/contract, additive-first per 04 §1.3). The UNIQUE(idempotency_key)
-- is the source of truth for effect-once: the caller inserts the key in the SAME
-- transaction as the business write and outbox row (D9 / 02 §3). Redis is a
-- read-through cache + IN_FLIGHT advisory only.
--
-- Partitioning/retention (24 h TTL of 02 §3) is left to the adopting slice's
-- housekeeping (a periodic DELETE or a time-partition drop); the core semantics
-- need only the table + unique constraint.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    idempotency_key TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'IN_FLIGHT',
    response_code   INTEGER,
    response_body   BYTEA,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (idempotency_key)
);

-- The PRIMARY KEY already enforces UNIQUE(idempotency_key); the explicit name
-- documents intent and lets slices reference it. An index on created_at helps
-- the TTL sweep.
CREATE INDEX IF NOT EXISTS idempotency_keys_created_at_idx
    ON idempotency_keys (created_at);
