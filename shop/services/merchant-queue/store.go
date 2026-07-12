package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	"github.com/shop-platform/shop/libs/inbox"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one order/cart/merchant-catalog use)
)

//go:embed migrations/0001_merchant_queue.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with order's
// PGSchema() / merchant-catalog's). The runtime here uses in-memory SQLite
// (process-mode sandbox; no PG daemon — disclosed in VERIFICATION §V-T11); the
// projection / LWW / rebuild SEMANTICS are engine-agnostic and identical.
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_merchant_queue.pg.sql (types
// only differ: TIMESTAMPTZ→TIMESTAMP, BIGINT→INTEGER, BIGSERIAL→INTEGER
// AUTOINCREMENT). The read model, the append-only event log, and the capacity
// config are otherwise identical to production.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS incoming_orders (
    order_id      TEXT NOT NULL PRIMARY KEY,
    merchant_id   TEXT NOT NULL DEFAULT '',
    shard         INTEGER NOT NULL DEFAULT -1,
    cell          INTEGER NOT NULL DEFAULT -1,
    customer_id   TEXT NOT NULL DEFAULT '',
    total_minor   INTEGER NOT NULL DEFAULT 0,
    currency      TEXT NOT NULL DEFAULT '',
    queue_state   TEXT NOT NULL DEFAULT 'CREATED',
    phase         INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMP,
    paid_at       TIMESTAMP,
    accepted_at   TIMESTAMP,
    last_event_at TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS incoming_orders_merchant_idx ON incoming_orders (merchant_id, queue_state);
CREATE INDEX IF NOT EXISTS incoming_orders_cell_idx ON incoming_orders (cell);

CREATE TABLE IF NOT EXISTS order_event_log (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id    TEXT NOT NULL,
    order_id    TEXT NOT NULL,
    merchant_id TEXT NOT NULL DEFAULT '',
    event_type  TEXT NOT NULL,
    phase       INTEGER NOT NULL,
    occurred_at TIMESTAMP,
    payload     TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS order_event_log_order_idx ON order_event_log (order_id, seq);

CREATE TABLE IF NOT EXISTS kitchen_capacity (
    merchant_id        TEXT NOT NULL PRIMARY KEY,
    accepts_per_window INTEGER NOT NULL DEFAULT 30,
    window_seconds     INTEGER NOT NULL DEFAULT 600,
    updated_at         TIMESTAMP NOT NULL
);
`

// QueueRow is one projected incoming-order (the read model).
type QueueRow struct {
	OrderID     string    `json:"order_id"`
	MerchantID  string    `json:"merchant_id"`
	Shard       int       `json:"shard"`
	Cell        int       `json:"cell"`
	CustomerID  string    `json:"customer_id,omitempty"`
	TotalAmount int64     `json:"total_minor"`
	Currency    string    `json:"currency,omitempty"`
	QueueState  string    `json:"queue_state"`
	Phase       int       `json:"phase"`
	CreatedAt   time.Time `json:"-"`
	PaidAt      time.Time `json:"-"`
	AcceptedAt  time.Time `json:"-"`
	LastEventAt time.Time `json:"-"`
}

// projectedEvent is the decoded, phase-tagged form of an order.* event that the
// fold operates on (identical whether it comes off the bus live or is replayed
// from the log during a rebuild).
type projectedEvent struct {
	EventID    string
	OrderID    string
	MerchantID string
	CustomerID string
	Amount     int64
	Currency   string
	EventType  string
	Phase      int
	State      string
	OccurredAt time.Time
	RawPayload string
}

// project decodes an envelope into a projectedEvent, or ok=false for a topic the
// queue does not project (forward-compat).
func project(eventID, eventType string, occurredAt time.Time, p orderPayload, raw string) (projectedEvent, bool) {
	phase, state, ok := phaseFor(eventType)
	if !ok {
		return projectedEvent{}, false
	}
	ev := projectedEvent{
		EventID:    eventID,
		OrderID:    p.OrderID,
		MerchantID: p.MerchantID,
		CustomerID: p.CustomerID,
		EventType:  eventType,
		Phase:      phase,
		State:      state,
		OccurredAt: occurredAt,
		RawPayload: raw,
	}
	if p.Total != nil {
		ev.Amount = p.Total.Amount
		ev.Currency = p.Total.Currency
	}
	return ev, true
}

// store is the merchant-queue persistence: the read model, the append-only event
// log (rebuild source), the capacity config, and — migrated alongside (one PG
// per service, 01 §1) — the consumer inbox (exactly-once projection).
type store struct {
	db     *sql.DB
	region string
	inbx   *inbox.Processor
}

// openStore builds an in-memory-SQLite-backed store with the inbox migrated.
// maxConns=1 serialises the single in-memory writer (as order/cart do).
func openStore(ctx context.Context, region string) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("merchant-queue migrate: %w", err)
	}
	if err := inbox.Migrate(ctx, db, inbox.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("inbox migrate: %w", err)
	}
	return &store{
		db:     db,
		region: region,
		inbx:   inbox.NewProcessor(db, inbox.SQLiteDialect{}, "merchant-queue"),
	}, nil
}

func (s *store) close() { _ = s.db.Close() }

// applyModelTx folds one projectedEvent onto the read model INSIDE tx. It is the
// single fold rule, shared by the live projection and the rebuild:
//   - create the row on first sight of an order;
//   - backfill merchant_id + shard + cell the moment an event carries it (D11);
//   - advance queue_state/phase ONLY when the event's phase is strictly greater
//     (LWW forward-only) — so a duplicate or out-of-order delivery is a no-op on
//     the state, and cancelled (phase 99) always wins over any non-terminal.
func (s *store) applyModelTx(ctx context.Context, tx *sql.Tx, ev projectedEvent, now time.Time) error {
	var (
		curPhase   int
		curMerch   string
		exists     bool
	)
	err := tx.QueryRowContext(ctx, `SELECT phase, merchant_id FROM incoming_orders WHERE order_id = ?`, ev.OrderID).
		Scan(&curPhase, &curMerch)
	switch {
	case err == sql.ErrNoRows:
		exists = false
	case err != nil:
		return err
	default:
		exists = true
	}

	shard, cell := -1, -1
	if ev.MerchantID != "" {
		shard = logicalShard(ev.MerchantID)
		cell = cellFor(ev.MerchantID)
	}

	if !exists {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO incoming_orders
			   (order_id, merchant_id, shard, cell, customer_id, total_minor, currency,
			    queue_state, phase, created_at, paid_at, accepted_at, last_event_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.OrderID, ev.MerchantID, shard, cell, ev.CustomerID, ev.Amount, ev.Currency,
			ev.State, ev.Phase,
			tsOrNil(ev.EventType == TopicOrderCreated, ev.OccurredAt),
			tsOrNil(ev.EventType == TopicOrderPaid, ev.OccurredAt),
			tsOrNil(ev.EventType == TopicOrderAccepted, ev.OccurredAt),
			ev.OccurredAt, now)
		return err
	}

	// Backfill merchant_id/shard/cell if we now know it and didn't before.
	if curMerch == "" && ev.MerchantID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE incoming_orders SET merchant_id = ?, shard = ?, cell = ? WHERE order_id = ?`,
			ev.MerchantID, shard, cell, ev.OrderID); err != nil {
			return err
		}
	}
	// Fill customer/total if a later event carries them and they were unset.
	if ev.CustomerID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE incoming_orders SET customer_id = ? WHERE order_id = ? AND customer_id = ''`,
			ev.CustomerID, ev.OrderID); err != nil {
			return err
		}
	}
	if ev.Amount != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE incoming_orders SET total_minor = ?, currency = ? WHERE order_id = ? AND total_minor = 0`,
			ev.Amount, ev.Currency, ev.OrderID); err != nil {
			return err
		}
	}
	// Stamp the lifecycle timestamps this event carries.
	switch ev.EventType {
	case TopicOrderPaid:
		if _, err := tx.ExecContext(ctx, `UPDATE incoming_orders SET paid_at = ? WHERE order_id = ? AND paid_at IS NULL`, ev.OccurredAt, ev.OrderID); err != nil {
			return err
		}
	case TopicOrderAccepted:
		if _, err := tx.ExecContext(ctx, `UPDATE incoming_orders SET accepted_at = ? WHERE order_id = ? AND accepted_at IS NULL`, ev.OccurredAt, ev.OrderID); err != nil {
			return err
		}
	case TopicOrderCreated:
		if _, err := tx.ExecContext(ctx, `UPDATE incoming_orders SET created_at = ? WHERE order_id = ? AND created_at IS NULL`, ev.OccurredAt, ev.OrderID); err != nil {
			return err
		}
	}
	// LWW: advance state/phase only forward.
	if ev.Phase > curPhase {
		if _, err := tx.ExecContext(ctx,
			`UPDATE incoming_orders SET queue_state = ?, phase = ?, last_event_at = ?, updated_at = ? WHERE order_id = ?`,
			ev.State, ev.Phase, ev.OccurredAt, now, ev.OrderID); err != nil {
			return err
		}
	} else {
		// keep last_event_at monotone by wall-arrival for freshness bookkeeping
		if _, err := tx.ExecContext(ctx,
			`UPDATE incoming_orders SET updated_at = ? WHERE order_id = ?`, now, ev.OrderID); err != nil {
			return err
		}
	}
	return nil
}

// logEventTx appends the projected event to the append-only rebuild log INSIDE
// tx (so the log row and the model apply commit atomically on the inbox tx).
func (s *store) logEventTx(ctx context.Context, tx *sql.Tx, ev projectedEvent) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO order_event_log (event_id, order_id, merchant_id, event_type, phase, occurred_at, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.EventID, ev.OrderID, ev.MerchantID, ev.EventType, ev.Phase, ev.OccurredAt, ev.RawPayload)
	return err
}

func tsOrNil(cond bool, t time.Time) any {
	if cond {
		return t
	}
	return nil
}

// getRow reads one read-model row.
func (s *store) getRow(ctx context.Context, orderID string) (QueueRow, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT order_id, merchant_id, shard, cell, customer_id, total_minor, currency, queue_state, phase
		   FROM incoming_orders WHERE order_id = ?`, orderID)
	var r QueueRow
	err := row.Scan(&r.OrderID, &r.MerchantID, &r.Shard, &r.Cell, &r.CustomerID, &r.TotalAmount, &r.Currency, &r.QueueState, &r.Phase)
	if err == sql.ErrNoRows {
		return QueueRow{}, false, nil
	}
	if err != nil {
		return QueueRow{}, false, err
	}
	return r, true, nil
}

// listQueue returns a merchant's queue rows in a given state (state="" = all).
func (s *store) listQueue(ctx context.Context, merchantID, state string) ([]QueueRow, error) {
	q := `SELECT order_id, merchant_id, shard, cell, customer_id, total_minor, currency, queue_state, phase
	        FROM incoming_orders WHERE merchant_id = ?`
	args := []any{merchantID}
	if state != "" {
		q += ` AND queue_state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY order_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueRow{}
	for rows.Next() {
		var r QueueRow
		if err := rows.Scan(&r.OrderID, &r.MerchantID, &r.Shard, &r.Cell, &r.CustomerID, &r.TotalAmount, &r.Currency, &r.QueueState, &r.Phase); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// pendingCount returns how many of a merchant's orders are awaiting accept.
func (s *store) pendingCount(ctx context.Context, merchantID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incoming_orders WHERE merchant_id = ? AND queue_state = ?`,
		merchantID, StatePending).Scan(&n)
	return n, err
}

// snapshot returns the full read model keyed by order_id (parity comparisons).
func (s *store) snapshot(ctx context.Context) (map[string]QueueRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT order_id, merchant_id, shard, cell, customer_id, total_minor, currency, queue_state, phase FROM incoming_orders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]QueueRow{}
	for rows.Next() {
		var r QueueRow
		if err := rows.Scan(&r.OrderID, &r.MerchantID, &r.Shard, &r.Cell, &r.CustomerID, &r.TotalAmount, &r.Currency, &r.QueueState, &r.Phase); err != nil {
			return nil, err
		}
		out[r.OrderID] = r
	}
	return out, rows.Err()
}

// orderCount / logCount are audit counters.
func (s *store) orderCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incoming_orders`).Scan(&n)
	return n, err
}

func (s *store) logCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_event_log`).Scan(&n)
	return n, err
}

// cellCounts returns per-cell row counts (to find the largest cell for a rebuild).
func (s *store) cellCounts(ctx context.Context) (map[int]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cell, COUNT(*) FROM incoming_orders GROUP BY cell`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var cell, n int
		if err := rows.Scan(&cell, &n); err != nil {
			return nil, err
		}
		out[cell] = n
	}
	return out, rows.Err()
}

// RebuildResult reports a rebuild run (D7 Tier-1 rebuild-from-events).
type RebuildResult struct {
	Cell       int           `json:"cell"` // -1 = whole store
	Orders     int           `json:"orders"`
	Events     int           `json:"events"`
	Duration   time.Duration `json:"-"`
	DurationMs int64         `json:"duration_ms"`
	ParityOK   bool          `json:"parity_ok"`
	Mismatches int           `json:"mismatches"`
}

// Rebuild reconstructs the WHOLE read model from the append-only event log and
// asserts the rebuilt model equals the live one (100% parity). cell >= 0 rebuilds
// ONLY that physical cell (the "rebuild of the largest cell" drill) and compares
// only that cell's rows. The rebuild replays the log in seq order through the
// SAME fold (applyModelTx) with no re-logging — the read model is a pure fold, so
// this is deterministic.
func (s *store) Rebuild(ctx context.Context, cell int, now time.Time) (RebuildResult, error) {
	// Capture the live model for the parity comparison BEFORE we clear it.
	live, err := s.snapshot(ctx)
	if err != nil {
		return RebuildResult{}, err
	}

	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RebuildResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Clear the target rows.
	if cell >= 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM incoming_orders WHERE cell = ?`, cell); err != nil {
			return RebuildResult{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM incoming_orders`); err != nil {
			return RebuildResult{}, err
		}
	}

	// Replay the log in seq order.
	rows, err := tx.QueryContext(ctx,
		`SELECT event_id, order_id, merchant_id, event_type, phase, occurred_at, payload FROM order_event_log ORDER BY seq ASC`)
	if err != nil {
		return RebuildResult{}, err
	}
	type logRow struct {
		eventID, orderID, merchantID, eventType, payload string
		phase                                            int
		occurredAt                                       time.Time
	}
	var logRows []logRow
	for rows.Next() {
		var lr logRow
		var occ sql.NullTime
		if err := rows.Scan(&lr.eventID, &lr.orderID, &lr.merchantID, &lr.eventType, &lr.phase, &occ, &lr.payload); err != nil {
			rows.Close()
			return RebuildResult{}, err
		}
		if occ.Valid {
			lr.occurredAt = occ.Time.UTC()
		}
		logRows = append(logRows, lr)
	}
	rows.Close()

	events := 0
	for _, lr := range logRows {
		if cell >= 0 && cellFor(lr.merchantID) != cell {
			continue // per-cell rebuild replays only that cell's events
		}
		p, err := decodeOrderPayload([]byte(lr.payload))
		if err != nil {
			return RebuildResult{}, err
		}
		ev, ok := project(lr.eventID, lr.eventType, lr.occurredAt, p, lr.payload)
		if !ok {
			continue
		}
		if err := s.applyModelTx(ctx, tx, ev, now); err != nil {
			return RebuildResult{}, err
		}
		events++
	}
	if err := tx.Commit(); err != nil {
		return RebuildResult{}, err
	}

	// Parity: the rebuilt model must equal the live model (over the target cell).
	rebuilt, err := s.snapshot(ctx)
	if err != nil {
		return RebuildResult{}, err
	}
	mismatches := diffModels(live, rebuilt, cell)
	orders := len(rebuilt)
	if cell >= 0 {
		orders = 0
		for _, r := range rebuilt {
			if r.Cell == cell {
				orders++
			}
		}
	}
	dur := time.Since(start)
	return RebuildResult{
		Cell:       cell,
		Orders:     orders,
		Events:     events,
		Duration:   dur,
		DurationMs: dur.Milliseconds(),
		ParityOK:   mismatches == 0,
		Mismatches: mismatches,
	}, nil
}

// diffModels counts orders whose (queue_state, merchant_id, shard, cell) differ
// between two snapshots. When cell >= 0 only that cell's orders are compared.
func diffModels(a, b map[string]QueueRow, cell int) int {
	mismatches := 0
	seen := map[string]bool{}
	cmp := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		ra, oka := a[id]
		rb, okb := b[id]
		if cell >= 0 {
			ca, cb := ra.Cell, rb.Cell
			if oka && ca != cell {
				oka = false
			}
			if okb && cb != cell {
				okb = false
			}
			if !oka && !okb {
				return
			}
		}
		if oka != okb || ra.QueueState != rb.QueueState || ra.MerchantID != rb.MerchantID || ra.Shard != rb.Shard || ra.Cell != rb.Cell {
			mismatches++
		}
	}
	for id := range a {
		cmp(id)
	}
	for id := range b {
		cmp(id)
	}
	return mismatches
}

// refFoldState is the INDEPENDENT reference fold used by the parity test: given a
// per-order canonical event sequence it computes the expected (state, phase,
// merchant). It shares no code with applyModelTx (different implementation) so a
// match proves the projection is correct, not just self-consistent.
func refFoldState(events []projectedEvent) map[string]QueueRow {
	out := map[string]QueueRow{}
	for _, e := range events {
		r := out[e.OrderID]
		r.OrderID = e.OrderID
		if r.MerchantID == "" && e.MerchantID != "" {
			r.MerchantID = e.MerchantID
			r.Shard = logicalShard(e.MerchantID)
			r.Cell = cellFor(e.MerchantID)
		}
		if e.Phase > r.Phase {
			r.Phase = e.Phase
			r.QueueState = e.State
		}
		out[e.OrderID] = r
	}
	return out
}
