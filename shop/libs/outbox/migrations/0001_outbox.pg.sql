-- 0001_outbox.pg.sql — D8 transactional outbox (PostgreSQL, production).
--
-- Time-partitioned by day (native range partitioning). The relay tails by the
-- monotonic `id` with a durable cursor (outbox_relay_cursor); cleanup = DROP
-- the partitions whose day is fully behind the cursor. There is NO
-- `published boolean` column and NO per-row UPDATE — that poller pattern is
-- banned (D8) because it churns dead tuples and triggers vacuum storms at this
-- write rate. In production Debezium reads the WAL of this table
-- (deploy/cdc/debezium-connector.json); the id-tail is the sqlite/mem stand-in.

CREATE TABLE IF NOT EXISTS outbox (
    id          BIGINT       GENERATED ALWAYS AS IDENTITY,
    event_id    TEXT         NOT NULL,
    topic       TEXT         NOT NULL,
    agg_key     TEXT         NOT NULL,           -- partition key = aggregate id (D5)
    payload     BYTEA        NOT NULL,           -- marshaled 02 §4.3 envelope
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    part_day    DATE         NOT NULL,           -- range-partition key
    PRIMARY KEY (id, part_day)
) PARTITION BY RANGE (part_day);

-- Debezium/CDC ordering key: an index on id speeds the tail range scan and the
-- WAL consumer's snapshot.
CREATE INDEX IF NOT EXISTS outbox_id_idx ON outbox (id);

-- Example daily partitions. In production a scheduled job (or pg_partman)
-- pre-creates tomorrow's partition and DROPs partitions older than retention;
-- DROP is O(1) and produces zero dead tuples (vs. a DELETE sweep).
--   CREATE TABLE outbox_2026_07_11 PARTITION OF outbox
--     FOR VALUES FROM ('2026-07-11') TO ('2026-07-12');
--   ...
--   DROP TABLE outbox_2026_07_04;   -- partition-drop cleanup

-- Durable relay cursor (one row per relay / consumer path). Tiny, hot, no churn.
CREATE TABLE IF NOT EXISTS outbox_relay_cursor (
    relay_name  TEXT         NOT NULL PRIMARY KEY,
    last_id     BIGINT       NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
