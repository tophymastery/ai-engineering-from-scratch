-- 0001_location.pg.sql — V-T13 Driver telemetry plane (Location team; D14/D15).
-- PG keeps ONLY per-trip summary polylines (D15: "PG keeps per-trip summary
-- polylines only … PG at full rate is petabyte nonsense"). Live positions live in
-- the Redis H3 res-7 geo index (30 s TTL); raw frames are downsampled 1:10 into
-- Iceberg. Nothing on the ultra-hot per-ping path touches this table — a row is
-- written once per COMPLETED trip, so the per-cell PG write rate stays well under
-- the 500/s budget.
--
-- Partitioned by day (range) so old trips drop by partition, never by row DELETE
-- (the S-T6 partition-drop-cleanup convention). In this sandbox the runtime store
-- is in-memory SQLite (no PG daemon); this migration is the production schema and
-- is asserted at parity with the SQLite twin (TestSchemaParity).

CREATE TABLE IF NOT EXISTS trip_summaries (
    trip_id     TEXT        NOT NULL,
    driver_id   TEXT        NOT NULL,
    order_id    TEXT        NOT NULL DEFAULT '',
    points      BIGINT      NOT NULL DEFAULT 0,
    start_lat   DOUBLE PRECISION NOT NULL,
    start_lng   DOUBLE PRECISION NOT NULL,
    end_lat     DOUBLE PRECISION NOT NULL,
    end_lng     DOUBLE PRECISION NOT NULL,
    polyline    TEXT        NOT NULL DEFAULT '[]',
    started_at  TIMESTAMPTZ,
    ended_at    TIMESTAMPTZ,
    part_day    DATE        NOT NULL,
    PRIMARY KEY (trip_id, part_day)
) PARTITION BY RANGE (part_day);

CREATE INDEX IF NOT EXISTS trip_summaries_driver_idx ON trip_summaries (driver_id, ended_at);
CREATE INDEX IF NOT EXISTS trip_summaries_order_idx  ON trip_summaries (order_id);
