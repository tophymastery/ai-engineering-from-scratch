package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/idempotency"
	"github.com/shop-platform/shop/libs/inbox"
	"github.com/shop-platform/shop/libs/outbox"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one cart/pricing-promo use)
)

//go:embed migrations/0001_order.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with cart's
// PGSchema() / libs/idempotency Schema()). The runtime here uses in-memory
// SQLite (process-mode sandbox; no PG daemon — disclosed in VERIFICATION §V-T9);
// the durable-timer / event-store / idempotency / outbox / inbox SEMANTICS are
// engine-agnostic and identical on either.
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_order.pg.sql (types only
// differ: TIMESTAMPTZ→TIMESTAMP, BIGINT→INTEGER). The orders table, the
// append-only order_events event store, and the durable timers table are the
// heart of the saga's durability.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS orders (
    order_id     TEXT NOT NULL PRIMARY KEY,
    customer_id  TEXT NOT NULL,
    merchant_id  TEXT NOT NULL,
    quote_id     TEXT NOT NULL,
    region       TEXT NOT NULL,
    status       TEXT NOT NULL,
    total_minor  INTEGER NOT NULL,
    currency     TEXT NOT NULL,
    auth_id      TEXT NOT NULL DEFAULT '',
    capture_id   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);

-- order_events is the APPEND-ONLY event store (01 §6). Current state is a pure
-- fold over these rows, so any order can be replayed for audit / migration.
CREATE TABLE IF NOT EXISTS order_events (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id    TEXT NOT NULL,
    trigger     TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS order_events_order_idx ON order_events (order_id, seq);

-- timers is the DURABLE timer table (T_accept / T_dispatch / capture-by / the
-- PAYMENT_PENDING remediation timer). A leased sweeper fires each due timer
-- exactly once (timers.go). Surviving a process crash ⇒ the table + the lease,
-- not in-memory state, are the source of truth.
CREATE TABLE IF NOT EXISTS timers (
    timer_id     TEXT NOT NULL PRIMARY KEY,
    order_id     TEXT NOT NULL,
    kind         TEXT NOT NULL,
    trigger      TEXT NOT NULL,
    due_at       TIMESTAMP NOT NULL,
    status       TEXT NOT NULL DEFAULT 'PENDING',   -- PENDING | FIRING | FIRED | CANCELLED
    leased_by    TEXT NOT NULL DEFAULT '',
    leased_until TIMESTAMP,
    fired_at     TIMESTAMP,
    created_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS timers_due_idx ON timers (status, due_at);
CREATE INDEX IF NOT EXISTS timers_order_idx ON timers (order_id);
`

// money is the 02 §1 integer-minor-unit money value.
type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// OrderRow is the persisted order aggregate.
type OrderRow struct {
	OrderID    string
	CustomerID string
	MerchantID string
	QuoteID    string
	Region     string
	Status     State
	Total      money
	AuthID     string
	CaptureID  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// store is the order persistence layer: the orders table, the append-only
// order_events event store, the durable timers table, and — migrated alongside
// (one PG per service, 01 §1) — the D9 idempotency_keys table, the D22
// transactional outbox, and the consumer inbox.
type store struct {
	db      *sql.DB
	region  string
	clock   Clock
	dialect idempotency.Dialect

	idem *idempotency.Manager
	ob   *outbox.SQLStore
	inbx *inbox.Processor
}

// openStore builds an in-memory-SQLite-backed store with every lib table
// migrated. maxConns=1 serialises the single in-memory writer (as cart /
// pricing-promo / merchant-catalog do).
func openStore(ctx context.Context, region string, clock Clock) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("order migrate: %w", err)
	}
	if err := idempotency.Migrate(ctx, db, idempotency.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("idempotency migrate: %w", err)
	}
	if err := outbox.Migrate(ctx, db, outbox.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("outbox migrate: %w", err)
	}
	if err := inbox.Migrate(ctx, db, inbox.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("inbox migrate: %w", err)
	}
	st := &store{
		db:      db,
		region:  region,
		clock:   clock,
		dialect: idempotency.SQLiteDialect{},
		// D9: the durable Store is source of truth; a nil cache is a valid
		// pure-DB mode (correct, uncached). The advisory cache is the Redis
		// demotion — not needed for correctness (05 D9).
		idem: idempotency.New(idempotency.NewSQLStore(db, idempotency.SQLiteDialect{}), idempotency.NewMemCache()),
		ob:   outbox.NewSQLStore(db, outbox.SQLiteDialect{}),
		inbx: inbox.NewProcessor(db, inbox.SQLiteDialect{}, "order"),
	}
	return st, nil
}

func (s *store) close() { _ = s.db.Close() }

// --- orders CRUD -------------------------------------------------------------

// insertOrderTx writes a fresh order row inside the caller's idempotent tx
// (idempotency.Execer). It runs in the SAME transaction as the idempotency key
// insert (D9): the order and the effect-once key commit together or not at all.
func (s *store) insertOrderTx(ctx context.Context, tx idempotency.Execer, o OrderRow) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO orders (order_id, customer_id, merchant_id, quote_id, region, status,
		     total_minor, currency, auth_id, capture_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OrderID, o.CustomerID, o.MerchantID, o.QuoteID, o.Region, string(o.Status),
		o.Total.Amount, o.Total.Currency, o.AuthID, o.CaptureID, o.CreatedAt, o.UpdatedAt)
	return err
}

// appendEventTx appends one row to the order_events event store inside a tx
// (idempotency.Execer variant, used on the checkout path).
func (s *store) appendEventTx(ctx context.Context, tx idempotency.Execer, orderID string, trig Trigger, from, to State, detail map[string]any, at time.Time) error {
	dj, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx,
		`INSERT INTO order_events (order_id, trigger, from_state, to_state, detail_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		orderID, string(trig), string(from), string(to), string(dj), at)
	return err
}

// appendEventSQLTx appends an order_events row inside a raw *sql.Tx (used by the
// saga on non-checkout transitions where the order owns the tx directly).
func (s *store) appendEventSQLTx(ctx context.Context, tx *sql.Tx, orderID string, trig Trigger, from, to State, detail map[string]any, at time.Time) error {
	dj, _ := json.Marshal(detail)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO order_events (order_id, trigger, from_state, to_state, detail_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		orderID, string(trig), string(from), string(to), string(dj), at)
	return err
}

// getOrder reads an order by id.
func (s *store) getOrder(ctx context.Context, orderID string) (OrderRow, bool, error) {
	var o OrderRow
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT order_id, customer_id, merchant_id, quote_id, region, status,
		        total_minor, currency, auth_id, capture_id, created_at, updated_at
		   FROM orders WHERE order_id = ?`, orderID).
		Scan(&o.OrderID, &o.CustomerID, &o.MerchantID, &o.QuoteID, &o.Region, &status,
			&o.Total.Amount, &o.Total.Currency, &o.AuthID, &o.CaptureID, &o.CreatedAt, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return OrderRow{}, false, nil
	}
	if err != nil {
		return OrderRow{}, false, err
	}
	o.Status = State(status)
	return o, true, nil
}

// orderCount / eventCount are audit counters read by the exactly-once tests.
func (s *store) orderCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders`).Scan(&n)
	return n, err
}

// transitionCount returns how many times a given trigger was applied to an order
// (the exactly-once assertion: a redelivered payment.authorized ⇒ still 1).
func (s *store) transitionCount(ctx context.Context, orderID string, trig Trigger) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM order_events WHERE order_id = ? AND trigger = ?`,
		orderID, string(trig)).Scan(&n)
	return n, err
}

// FoldState recomputes the current state PURELY from the order_events log
// (01 §6: current state is a fold over events). Used by the replay/audit test to
// prove the event store is the source of truth.
func (s *store) FoldState(ctx context.Context, orderID string) (State, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT to_state FROM order_events WHERE order_id = ? ORDER BY seq ASC`, orderID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	st := State("")
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			return "", err
		}
		st = State(to)
	}
	return st, rows.Err()
}

// --- outbox staging inside the idempotent tx --------------------------------

// stageEventTx inserts one 02 §4.3 envelope into the outbox INSIDE the caller's
// idempotent transaction (idempotency.Execer), so the event row commits
// atomically with the order write + the idempotency key (01 §3 / D22): a double
// tap or a BFF retry can never produce a second event. Mirrors
// outbox.SQLStore.WriteInTx exactly (same 6 columns, same envelope validation),
// specialised to the Execer the idempotency Manager hands us (it owns the tx).
func (s *store) stageEventTx(ctx context.Context, tx idempotency.Execer, topic string, env eventbus.Envelope) error {
	raw, err := env.Marshal()
	if err != nil {
		return err
	}
	if err := eventbus.ValidateEnvelope(raw); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	now := s.clock.Now()
	_, err = tx.Exec(ctx,
		`INSERT INTO outbox (event_id, topic, agg_key, payload, created_at, part_day) VALUES (?, ?, ?, ?, ?, ?)`,
		env.EventID, topic, env.PartitionKey(), raw, now, now.UTC().Format("2006-01-02"))
	return err
}

// outboxCount / outboxCountTopic are audit counters (exactly-once event proof).
func (s *store) outboxCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n, err
}

func (s *store) outboxCountTopic(ctx context.Context, topic string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE topic = ?`, topic).Scan(&n)
	return n, err
}
