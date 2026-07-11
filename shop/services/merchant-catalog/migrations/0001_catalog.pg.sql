-- 0001_catalog.pg.sql — PostgreSQL schema for the V-T3 merchant-catalog slice
-- (01 §1: "Merchants, stores, menus, items, availability, opening hours"; one PG
-- per service). The SQLite twin used by tests lives inline in store.go; only the
-- column TYPES differ (TIMESTAMPTZ vs TIMESTAMP). Additive, expand/contract-only
-- migrations per 04 §1.3.
--
-- Optimistic concurrency (02 §1): every MUTABLE resource (menu, store_status)
-- carries a monotonic `version` integer. The service derives the HTTP `ETag`
-- from it; a PATCH/PUT whose `If-Match` no longer equals the current version's
-- ETag is a stale write and is rejected with 412 STALE_WRITE — the headline
-- correctness property of this slice.

CREATE TABLE IF NOT EXISTS merchants (
    merchant_id  TEXT NOT NULL PRIMARY KEY,   -- mer_ prefixed ULID (02 §1)
    name         TEXT NOT NULL,
    region       TEXT NOT NULL DEFAULT 'local',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One menu aggregate per merchant. `version` is the optimistic-concurrency token
-- for the whole menu (items hang off it); it bumps on every accepted edit.
CREATE TABLE IF NOT EXISTS menus (
    merchant_id  TEXT NOT NULL PRIMARY KEY REFERENCES merchants(merchant_id),
    version      BIGINT NOT NULL DEFAULT 1,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS menu_items (
    item_id        TEXT NOT NULL PRIMARY KEY,  -- itm_ prefixed ULID
    merchant_id    TEXT NOT NULL REFERENCES merchants(merchant_id),
    name           TEXT NOT NULL,
    price_amount   BIGINT NOT NULL,            -- integer minor units (02 §1 Money)
    price_currency TEXT NOT NULL,              -- ISO currency
    available      BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS menu_items_merchant_idx ON menu_items (merchant_id);

-- Store status (OPEN/BUSY/CLOSED) is a second independently-versioned mutable
-- resource. Its own `version` drives its own ETag, so toggling the store status
-- and editing the menu never contend on one another's If-Match.
CREATE TABLE IF NOT EXISTS store_status (
    merchant_id  TEXT NOT NULL PRIMARY KEY REFERENCES merchants(merchant_id),
    status       TEXT NOT NULL DEFAULT 'CLOSED',  -- OPEN | BUSY | CLOSED
    version      BIGINT NOT NULL DEFAULT 1,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
