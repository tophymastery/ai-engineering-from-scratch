-- 0001_profile.pg.sql — identity-profile production schema (PostgreSQL), D3.
--
-- ONE such database exists PER JURISDICTION CELL (in-country for ID/VN). PII is
-- only ever stored ENCRYPTED, in the *_ct columns below (AES-256-GCM under a
-- per-user DEK; see crypto.go). Plaintext PII never touches disk.
--
-- PII-column convention (enforced by tools/piiscan): every column that holds
-- personal data is a `*_ct` ciphertext column AND is annotated `-- pii:<class>`.
-- The PII scanner parses these markers and FAILS CI if any is missing from the
-- checked-in data-inventory.yaml (the "unregistered PII table => CI red" rule).
-- Non-PII columns (tokens, jurisdiction, labels, timestamps) carry no marker.

CREATE TABLE IF NOT EXISTS profiles (
    user_token   TEXT        NOT NULL PRIMARY KEY,           -- usr_<ulid>, never PII
    jurisdiction TEXT        NOT NULL,                       -- owning cell: ID | VN | SG | ...
    full_name_ct TEXT,                                       -- pii:name        (encrypted)
    phone_ct     TEXT,                                       -- pii:phone       (encrypted)
    email_ct     TEXT,                                       -- pii:email       (encrypted)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    erased_at    TIMESTAMPTZ                                 -- set on crypto-shred erasure
);

CREATE TABLE IF NOT EXISTS addresses (
    addr_token   TEXT        NOT NULL PRIMARY KEY,           -- adr_<ulid>, never PII
    user_token   TEXT        NOT NULL REFERENCES profiles (user_token),
    jurisdiction TEXT        NOT NULL,
    label        TEXT        NOT NULL DEFAULT 'home',        -- non-PII tag
    line1_ct     TEXT,                                       -- pii:address     (encrypted)
    city_ct      TEXT,                                       -- pii:address     (encrypted)
    postal_ct    TEXT,                                       -- pii:address     (encrypted)
    geo_ct       TEXT,                                       -- pii:geo          (encrypted)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    erased_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS addresses_user_idx ON addresses (user_token);

-- data_keys is the KEYSTORE: the single wrapped copy of each user's DEK. This is
-- the crypto-shred target — it is deliberately the ONE store small and mutable
-- enough to hard-delete from (including its own backups) within the 72 h SLA.
-- Erasure NULLs wrapped_dek and stamps destroyed_at; the ciphertext everywhere
-- else is then permanently unreadable. Backups of PII stores do NOT carry this
-- table.
CREATE TABLE IF NOT EXISTS data_keys (
    user_token   TEXT        NOT NULL PRIMARY KEY,
    wrapped_dek  TEXT,                                       -- KEK-wrapped DEK; NULL once shredded
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    destroyed_at TIMESTAMPTZ
);
