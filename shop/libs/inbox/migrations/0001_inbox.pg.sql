-- 0001_inbox.pg.sql — D8 consumer inbox + D22 DLQ (PostgreSQL, production).
--
-- Both tables are RANGE-partitioned by day. Cleanup = DROP PARTITION:
--   * inbox retains 7 days (InboxRetention) — a redelivery older than that is
--     astronomically unlikely given the bus's 7 d hot tier.
--   * dlq retains replayed rows briefly for audit; parked rows are kept until an
--     operator replays or discards them.
-- DROP PARTITION is O(1) and leaves zero dead tuples (vs. a churny DELETE).

-- Consumer inbox: exactly-once effect. UNIQUE(consumer_group, event_id) is the
-- dedupe key; the row is inserted in the SAME tx as the handler's side effects.
CREATE TABLE IF NOT EXISTS inbox (
    event_id       TEXT        NOT NULL,
    consumer_group TEXT        NOT NULL,
    part_day       DATE        NOT NULL,
    processed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer_group, event_id, part_day)
) PARTITION BY RANGE (part_day);
--   CREATE TABLE inbox_2026_07_11 PARTITION OF inbox
--     FOR VALUES FROM ('2026-07-11') TO ('2026-07-12');
--   DROP TABLE inbox_2026_07_04;   -- 7-day retention drop

-- Per-consumer-group dead-letter queue (D22). Parked inline by the bus after N
-- failed attempts; dlqctl lists/inspects/replays. Replay re-emits via the
-- outbox so reprocessing converges exactly-once through the inbox above.
CREATE TABLE IF NOT EXISTS dlq (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY,
    event_id       TEXT        NOT NULL,
    consumer_group TEXT        NOT NULL,
    topic          TEXT        NOT NULL,
    agg_key        TEXT        NOT NULL,
    payload        BYTEA       NOT NULL,
    attempts       INTEGER     NOT NULL,
    cause          TEXT        NOT NULL DEFAULT '',
    status         TEXT        NOT NULL DEFAULT 'parked',   -- parked | replayed
    part_day       DATE        NOT NULL,
    parked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    replayed_at    TIMESTAMPTZ,
    PRIMARY KEY (id, part_day)
) PARTITION BY RANGE (part_day);

CREATE INDEX IF NOT EXISTS dlq_group_status_idx ON dlq (consumer_group, status);
