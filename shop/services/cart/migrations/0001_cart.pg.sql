-- 0001_cart.pg.sql — PostgreSQL schema for the V-T7 cart slice (01 §1: "Per-user
-- carts; item validation against catalog"; one PG per service). The SQLite twin
-- used by tests lives inline in store.go; only the column TYPES differ
-- (TIMESTAMPTZ vs TIMESTAMP, BOOLEAN vs INTEGER). Additive, expand/contract-only
-- migrations per 04 §1.3.
--
-- DURABLE STORE + REDIS SNAPSHOT (01 §1 "Redis snapshot + PG"): PostgreSQL is the
-- system of record for a cart and its lines; the Redis-like snapshot tier (an
-- in-process TTLStore in this sandbox — see snapshot.go) caches the assembled cart
-- view for fast reads and is rebuilt from PG on a miss (rehydrate).
--
-- Optimistic concurrency (02 §1): the cart is a MUTABLE resource carrying a
-- monotonic `version`. The service derives the HTTP `ETag` from it; an
-- add/remove whose `If-Match` no longer equals the current version's ETag is a
-- stale write, rejected with 412 STALE_WRITE — the headline concurrency property
-- of this slice (same compare-and-swap-in-tx pattern V-T3 proved).

CREATE TABLE IF NOT EXISTS carts (
    cart_id     TEXT NOT NULL PRIMARY KEY,   -- crt_ prefixed ULID (02 §1)
    user_token  TEXT NOT NULL DEFAULT '',    -- usr_ token only (D3: never PII)
    currency    TEXT NOT NULL DEFAULT '',    -- ISO currency of the cart (from its lines)
    version     BIGINT NOT NULL DEFAULT 1,   -- optimistic-concurrency token -> ETag
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per line item. The price is a SNAPSHOT of the catalog price at the time
-- the line was last (re)validated — integer minor units (02 §1 Money). Menu-change
-- revalidation (menu.updated consumer, see catalog.go) reprices `unit_amount` /
-- flips `available` in place and stamps `revalidated_at`, so a merchant's price or
-- availability change is reflected in the cart within the freshness window.
CREATE TABLE IF NOT EXISTS cart_items (
    cart_id        TEXT NOT NULL REFERENCES carts(cart_id),
    item_id        TEXT NOT NULL,              -- itm_ prefixed ULID
    merchant_id    TEXT NOT NULL,              -- mer_ prefixed ULID (which menu to revalidate against)
    name           TEXT NOT NULL,
    unit_amount    BIGINT NOT NULL,            -- catalog price snapshot, integer minor units
    unit_currency  TEXT NOT NULL,              -- ISO currency
    quantity       BIGINT NOT NULL,
    available      BOOLEAN NOT NULL DEFAULT true,  -- last-revalidated availability
    menu_version   BIGINT NOT NULL DEFAULT 0,      -- catalog menu version this line was priced against (LWW)
    revalidated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cart_id, item_id)
);
CREATE INDEX IF NOT EXISTS cart_items_merchant_idx ON cart_items (merchant_id, item_id);
