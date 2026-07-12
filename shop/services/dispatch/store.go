package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shop-platform/shop/libs/inbox"
	match "github.com/shop-platform/shop/services/dispatch/match"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one order/cart/merchant-queue use)
)

//go:embed migrations/0001_dispatch.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with order's /
// merchant-queue's PGSchema()). The runtime here uses in-memory SQLite
// (process-mode sandbox; no PG daemon — disclosed in VERIFICATION §V-T12); the
// snapshot-log + assignment SEMANTICS are engine-agnostic and identical.
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_dispatch.pg.sql (types only
// differ: TIMESTAMPTZ→TIMESTAMP, BIGINT→INTEGER, BIGSERIAL→INTEGER AUTOINCREMENT).
// The deterministic snapshot log + the assignment read model are otherwise
// identical to production.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS dispatch_snapshots (
    tick_id     INTEGER NOT NULL PRIMARY KEY,
    zone_key    TEXT NOT NULL,
    partition   INTEGER NOT NULL,
    at          TIMESTAMP,
    seed        INTEGER NOT NULL,
    n_orders    INTEGER NOT NULL DEFAULT 0,
    n_drivers   INTEGER NOT NULL DEFAULT 0,
    n_assigned  INTEGER NOT NULL DEFAULT 0,
    replay_ok   INTEGER NOT NULL DEFAULT 0,
    orders      TEXT NOT NULL DEFAULT '[]',
    drivers     TEXT NOT NULL DEFAULT '[]',
    assignments TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS dispatch_snapshots_zone_idx ON dispatch_snapshots (zone_key, tick_id);

CREATE TABLE IF NOT EXISTS assignments (
    order_id     TEXT NOT NULL PRIMARY KEY,
    driver_id    TEXT NOT NULL DEFAULT '',
    assignment_id TEXT NOT NULL DEFAULT '',
    pickup_eta_s INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'PENDING',
    zone_key     TEXT NOT NULL DEFAULT '',
    assigned_at  TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL
);
`

// store is the dispatch persistence: the queryable snapshot log + the assignment
// read model + the durable exactly-once inbox.
type store struct {
	db     *sql.DB
	region string
	inbx   *inbox.Processor
}

// openStore builds an in-memory-SQLite-backed store with the inbox migrated.
// maxConns=1 serialises the single in-memory writer (as order/merchant-queue do).
func openStore(ctx context.Context, region string) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("dispatch migrate: %w", err)
	}
	if err := inbox.Migrate(ctx, db, inbox.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("inbox migrate: %w", err)
	}
	return &store{db: db, region: region, inbx: inbox.NewProcessor(db, inbox.SQLiteDialect{}, "dispatch")}, nil
}

func (s *store) close() { _ = s.db.Close() }

// SnapshotRow is one persisted snapshot (the queryable log row).
type SnapshotRow struct {
	TickID      int64             `json:"tick_id"`
	ZoneKey     string            `json:"zone_key"`
	Partition   int               `json:"partition"`
	At          string            `json:"at"`
	Seed        int64             `json:"seed"`
	NOrders     int               `json:"n_orders"`
	NDrivers    int               `json:"n_drivers"`
	NAssigned   int               `json:"n_assigned"`
	ReplayOK    bool              `json:"replay_ok"`
	Assignments []match.Assignment `json:"assignments"`
}

// persistSnapshot writes one logged snapshot. replayOK records whether replaying
// it reproduced identical assignments (verified at persist time) — the durable
// evidence of the 100%-replay property, queryable after the fact.
func (s *store) persistSnapshot(ctx context.Context, snap match.Snapshot, replayOK bool) error {
	oj, _ := json.Marshal(snap.Orders)
	dj, _ := json.Marshal(snap.Drivers)
	aj, _ := json.Marshal(snap.Assignments)
	rok := 0
	if replayOK {
		rok = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dispatch_snapshots (tick_id, zone_key, partition, at, seed, n_orders, n_drivers, n_assigned, replay_ok, orders, drivers, assignments)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tick_id) DO NOTHING`,
		snap.TickID, snap.ZoneKey, snap.Partition, snap.At.UTC(), snap.Seed,
		len(snap.Orders), len(snap.Drivers), len(snap.Assignments), rok, string(oj), string(dj), string(aj))
	return err
}

// listSnapshots returns the most recent snapshots (queryable log).
func (s *store) listSnapshots(ctx context.Context, limit int) ([]SnapshotRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tick_id, zone_key, partition, at, seed, n_orders, n_drivers, n_assigned, replay_ok, assignments
		FROM dispatch_snapshots ORDER BY tick_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var r SnapshotRow
		var at time.Time
		var rok int
		var aj string
		if err := rows.Scan(&r.TickID, &r.ZoneKey, &r.Partition, &at, &r.Seed, &r.NOrders, &r.NDrivers, &r.NAssigned, &rok, &aj); err != nil {
			return nil, err
		}
		r.At = at.UTC().Format(time.RFC3339)
		r.ReplayOK = rok == 1
		_ = json.Unmarshal([]byte(aj), &r.Assignments)
		out = append(out, r)
	}
	return out, rows.Err()
}

// getSnapshotFull returns one persisted snapshot rehydrated into a domain
// Snapshot so it can be replayed on demand (GET /v1/admin/snapshots/{id}).
func (s *store) getSnapshotFull(ctx context.Context, tickID int64) (match.Snapshot, bool, error) {
	var (
		zoneKey        string
		partition      int
		at             time.Time
		seed           int64
		oj, dj, aj     string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT zone_key, partition, at, seed, orders, drivers, assignments
		FROM dispatch_snapshots WHERE tick_id = ?`, tickID).
		Scan(&zoneKey, &partition, &at, &seed, &oj, &dj, &aj)
	if err == sql.ErrNoRows {
		return match.Snapshot{}, false, nil
	}
	if err != nil {
		return match.Snapshot{}, false, err
	}
	snap := match.Snapshot{TickID: tickID, ZoneKey: zoneKey, Partition: partition, At: at.UTC(), Seed: seed}
	_ = json.Unmarshal([]byte(oj), &snap.Orders)
	_ = json.Unmarshal([]byte(dj), &snap.Drivers)
	_ = json.Unmarshal([]byte(aj), &snap.Assignments)
	return snap, true, nil
}

// snapshotCount is the number of persisted snapshots (test/telemetry helper).
func (s *store) snapshotCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatch_snapshots`).Scan(&n)
	return n, err
}

// upsertAssignment records/updates the assignment read model for an order.
func (s *store) upsertAssignment(ctx context.Context, a match.AssignedResult, assignmentID, zoneKey, status string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO assignments (order_id, driver_id, assignment_id, pickup_eta_s, status, zone_key, assigned_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(order_id) DO UPDATE SET
		  driver_id=excluded.driver_id, assignment_id=excluded.assignment_id,
		  pickup_eta_s=excluded.pickup_eta_s, status=excluded.status,
		  zone_key=excluded.zone_key, assigned_at=excluded.assigned_at, updated_at=excluded.updated_at`,
		a.OrderID, a.DriverID, assignmentID, a.PickupETA, status, zoneKey, a.AssignedAt.UTC(), now.UTC())
	return err
}

// AssignmentRow is the assignment read-model row.
type AssignmentRow struct {
	OrderID      string `json:"order_id"`
	DriverID     string `json:"driver_id"`
	AssignmentID string `json:"assignment_id"`
	PickupETA    int    `json:"pickup_eta_s"`
	Status       string `json:"status"`
	ZoneKey      string `json:"zone_key,omitempty"`
	AssignedAt   string `json:"assigned_at,omitempty"`
}

// getAssignment returns the assignment for an order, if any.
func (s *store) getAssignment(ctx context.Context, orderID string) (AssignmentRow, bool, error) {
	var (
		r  AssignmentRow
		at time.Time
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT order_id, driver_id, assignment_id, pickup_eta_s, status, zone_key, assigned_at
		FROM assignments WHERE order_id = ?`, orderID).
		Scan(&r.OrderID, &r.DriverID, &r.AssignmentID, &r.PickupETA, &r.Status, &r.ZoneKey, &at)
	if err == sql.ErrNoRows {
		return AssignmentRow{}, false, nil
	}
	if err != nil {
		return AssignmentRow{}, false, err
	}
	if !at.IsZero() {
		r.AssignedAt = at.UTC().Format(time.RFC3339)
	}
	return r, true, nil
}
