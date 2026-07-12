package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"

	plane "github.com/shop-platform/shop/services/location-gateway/plane"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one dispatch/order/cart use)
)

//go:embed migrations/0001_location.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with the other
// slices' PGSchema()). The runtime here uses in-memory SQLite (process-mode
// sandbox; no PG daemon — disclosed in VERIFICATION §V-T13); the trip-summary
// SEMANTICS are engine-agnostic and identical.
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_location.pg.sql (types only
// differ: TIMESTAMPTZ→TIMESTAMP, DOUBLE PRECISION→REAL, BIGINT→INTEGER, no native
// RANGE partitioning → a part_day column). PG keeps trip summaries ONLY (D15).
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS trip_summaries (
    trip_id     TEXT NOT NULL,
    driver_id   TEXT NOT NULL,
    order_id    TEXT NOT NULL DEFAULT '',
    points      INTEGER NOT NULL DEFAULT 0,
    start_lat   REAL NOT NULL,
    start_lng   REAL NOT NULL,
    end_lat     REAL NOT NULL,
    end_lng     REAL NOT NULL,
    polyline    TEXT NOT NULL DEFAULT '[]',
    started_at  TIMESTAMP,
    ended_at    TIMESTAMP,
    part_day    DATE NOT NULL,
    PRIMARY KEY (trip_id, part_day)
);
CREATE INDEX IF NOT EXISTS trip_summaries_driver_idx ON trip_summaries (driver_id, ended_at);
CREATE INDEX IF NOT EXISTS trip_summaries_order_idx  ON trip_summaries (order_id);
`

// store is the location-gateway persistence: the per-trip summary table only (the
// D15 "PG trip summaries only" rule). Raw positions never come here.
type store struct {
	db *sql.DB
}

// openStore opens an in-memory SQLite store and migrates the trip-summary schema.
func openStore(ctx context.Context) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one shared in-memory connection
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) close() { _ = s.db.Close() }

// writeTripSummary persists ONE per-trip summary row (the only PG write on this
// plane). polyline is the compact downsampled vertex list.
func (s *store) writeTripSummary(ctx context.Context, sum plane.TripSummary, polyline []plane.Position, day string) error {
	pl, _ := json.Marshal(polyline)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trip_summaries
		  (trip_id, driver_id, order_id, points, start_lat, start_lng, end_lat, end_lng, polyline, started_at, ended_at, part_day)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(trip_id, part_day) DO NOTHING`,
		sum.TripID, sum.DriverID, sum.OrderID, sum.Points,
		sum.StartLat, sum.StartLng, sum.EndLat, sum.EndLng, string(pl),
		sum.StartedAt, sum.EndedAt, day)
	return err
}

// tripSummaryCount returns the number of trip-summary rows (the PG-write proof:
// far fewer than raw positions).
func (s *store) tripSummaryCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trip_summaries`).Scan(&n)
	return n, err
}
