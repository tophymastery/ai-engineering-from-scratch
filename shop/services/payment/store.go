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

	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one order/cart/pricing use)
)

//go:embed migrations/0001_payment.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with order's
// PGSchema() / libs/idempotency Schema()). The runtime here uses in-memory
// SQLite (process-mode sandbox; no PG daemon — disclosed in VERIFICATION §V-T10);
// the D9 UNIQUE-key-in-tx / event-store / idempotency / outbox / inbox SEMANTICS
// are engine-agnostic and identical on either.
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_payment.pg.sql (types only
// differ: TIMESTAMPTZ→TIMESTAMP, BIGINT→INTEGER, JSONB→TEXT). The payments
// table (with UNIQUE(order_id) — the schema-level charge invariant), the
// append-only payment_events store, and the wallet ledger are the heart of the
// slice's money durability.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS payments (
    payment_id    TEXT NOT NULL PRIMARY KEY,
    order_id      TEXT NOT NULL,
    customer_id   TEXT NOT NULL,
    region        TEXT NOT NULL,
    method        TEXT NOT NULL DEFAULT 'card',
    amount_minor  INTEGER NOT NULL,
    currency      TEXT NOT NULL,
    status        TEXT NOT NULL,
    auth_id       TEXT NOT NULL DEFAULT '',
    capture_id    TEXT NOT NULL DEFAULT '',
    refund_id     TEXT NOT NULL DEFAULT '',
    psp           TEXT NOT NULL DEFAULT 'payment-sim',
    webhook_state TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS payments_order_uniq ON payments (order_id);
CREATE INDEX IF NOT EXISTS payments_status_idx ON payments (status, updated_at);
CREATE INDEX IF NOT EXISTS payments_auth_idx ON payments (auth_id);

CREATE TABLE IF NOT EXISTS payment_events (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    payment_id  TEXT NOT NULL,
    trigger     TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS payment_events_payment_idx ON payment_events (payment_id, seq);

CREATE TABLE IF NOT EXISTS wallets (
    customer_id   TEXT NOT NULL PRIMARY KEY,
    region        TEXT NOT NULL,
    balance_minor INTEGER NOT NULL DEFAULT 0,
    currency      TEXT NOT NULL DEFAULT 'THB',
    updated_at    TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS wallet_entries (
    entry_id    TEXT NOT NULL PRIMARY KEY,
    customer_id TEXT NOT NULL,
    payment_id  TEXT NOT NULL DEFAULT '',
    delta_minor INTEGER NOT NULL,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS wallet_entries_customer_idx ON wallet_entries (customer_id, created_at);
`

// money is the 02 §1 integer-minor-unit money value.
type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// PaymentRow is the persisted payment aggregate.
type PaymentRow struct {
	PaymentID    string
	OrderID      string
	CustomerID   string
	Region       string
	Method       string
	Amount       money
	Status       State
	AuthID       string
	CaptureID    string
	RefundID     string
	PSP          string
	WebhookState string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// store is the payment persistence layer: the payments table, the append-only
// payment_events store, the wallet ledger, and — migrated alongside (one PG per
// service, 01 §1) — the D9 idempotency_keys table, the D22 transactional outbox,
// and the consumer inbox.
type store struct {
	db      *sql.DB
	region  string
	clock   Clock
	dialect idempotency.Dialect

	idem  *idempotency.Manager
	cache *idempotency.SwappableCache // the DEMOTED Redis-like cache — droppable mid-storm (D9)
	ob    *outbox.SQLStore
	inbx  *inbox.Processor
}

// openStore builds an in-memory-SQLite-backed store with every lib table
// migrated. maxConns=1 serialises the single in-memory writer (as order / cart /
// pricing-promo do).
func openStore(ctx context.Context, region string, clock Clock) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("payment migrate: %w", err)
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
	// D9: the durable SQLStore is the source of truth. The advisory cache is the
	// Redis DEMOTION — a read-through replay accelerator + IN_FLIGHT marker only.
	// It is wrapped in a SwappableCache so a chaos drill / the failover-storm test
	// can DROP it mid-flight (the "forced Redis failover"): after Drop every money
	// mutation falls back to the PG UNIQUE path, which still guarantees one charge.
	cache := idempotency.NewSwappableCache(idempotency.NewMemCache())
	st := &store{
		db:      db,
		region:  region,
		clock:   clock,
		dialect: idempotency.SQLiteDialect{},
		idem:    idempotency.New(idempotency.NewSQLStore(db, idempotency.SQLiteDialect{}), cache),
		cache:   cache,
		ob:      outbox.NewSQLStore(db, outbox.SQLiteDialect{}),
		inbx:    inbox.NewProcessor(db, inbox.SQLiteDialect{}, "payment"),
	}
	return st, nil
}

func (s *store) close() { _ = s.db.Close() }

// --- payments CRUD -----------------------------------------------------------

// insertPaymentTx writes a fresh payment row inside the caller's idempotent tx
// (idempotency.Execer). It runs in the SAME transaction as the idempotency key
// insert (D9): the payment (the charge) and the effect-once key commit together
// or not at all. The UNIQUE(order_id) index is a second, schema-level guard —
// a duplicate order can never mint a second charge row.
func (s *store) insertPaymentTx(ctx context.Context, tx idempotency.Execer, p PaymentRow) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO payments (payment_id, order_id, customer_id, region, method,
		     amount_minor, currency, status, auth_id, capture_id, refund_id, psp, webhook_state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PaymentID, p.OrderID, p.CustomerID, p.Region, p.Method,
		p.Amount.Amount, p.Amount.Currency, string(p.Status), p.AuthID, p.CaptureID, p.RefundID, p.PSP, p.WebhookState, p.CreatedAt, p.UpdatedAt)
	return err
}

// appendEventTx appends one row to the payment_events store inside a tx
// (idempotency.Execer variant, used on the authorize path).
func (s *store) appendEventTx(ctx context.Context, tx idempotency.Execer, paymentID string, trig Trigger, from, to State, detail map[string]any, at time.Time) error {
	dj, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx,
		`INSERT INTO payment_events (payment_id, trigger, from_state, to_state, detail_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		paymentID, string(trig), string(from), string(to), string(dj), at)
	return err
}

// appendEventSQLTx appends a payment_events row inside a raw *sql.Tx (used by
// the capture/refund/void money mutations and the webhook/order-event consumers).
func (s *store) appendEventSQLTx(ctx context.Context, tx *sql.Tx, paymentID, trig string, from, to State, detail map[string]any, at time.Time) error {
	dj, _ := json.Marshal(detail)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO payment_events (payment_id, trigger, from_state, to_state, detail_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		paymentID, trig, string(from), string(to), string(dj), at)
	return err
}

// getPayment reads a payment by id.
func (s *store) getPayment(ctx context.Context, paymentID string) (PaymentRow, bool, error) {
	return s.scanPayment(s.db.QueryRowContext(ctx, paymentSelect+` WHERE payment_id = ?`, paymentID))
}

// getPaymentByOrder reads the single payment for an order (UNIQUE(order_id)).
func (s *store) getPaymentByOrder(ctx context.Context, orderID string) (PaymentRow, bool, error) {
	return s.scanPayment(s.db.QueryRowContext(ctx, paymentSelect+` WHERE order_id = ?`, orderID))
}

const paymentSelect = `SELECT payment_id, order_id, customer_id, region, method,
	        amount_minor, currency, status, auth_id, capture_id, refund_id, psp, webhook_state, created_at, updated_at
	   FROM payments`

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *store) scanPayment(row rowScanner) (PaymentRow, bool, error) {
	var p PaymentRow
	var status string
	err := row.Scan(&p.PaymentID, &p.OrderID, &p.CustomerID, &p.Region, &p.Method,
		&p.Amount.Amount, &p.Amount.Currency, &status, &p.AuthID, &p.CaptureID, &p.RefundID, &p.PSP, &p.WebhookState, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return PaymentRow{}, false, nil
	}
	if err != nil {
		return PaymentRow{}, false, err
	}
	p.Status = State(status)
	return p, true, nil
}

// --- audit counters (the money-correctness assertions) ----------------------

// paymentCount / paymentCountByStatus / distinctOrderCount are the row/charge
// counters the exactly-once + failover-storm tests read.
func (s *store) paymentCount(ctx context.Context) (int, error) {
	return s.count(ctx, `SELECT COUNT(*) FROM payments`)
}

func (s *store) distinctOrderCount(ctx context.Context) (int, error) {
	return s.count(ctx, `SELECT COUNT(DISTINCT order_id) FROM payments`)
}

func (s *store) paymentCountByStatus(ctx context.Context, status State) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments WHERE status = ?`, string(status)).Scan(&n)
	return n, err
}

// transitionCount returns how many payment_events rows carry a given trigger for
// a payment (the exactly-once assertion: a redelivered webhook ⇒ still 1).
func (s *store) transitionCount(ctx context.Context, paymentID, trig string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM payment_events WHERE payment_id = ? AND trigger = ?`,
		paymentID, trig).Scan(&n)
	return n, err
}

func (s *store) count(ctx context.Context, q string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

// FoldStatus recomputes the current status PURELY from payment_events (01 §6:
// current state is a fold over events), used by the replay/audit test to prove
// the event store is the money source of truth.
func (s *store) FoldStatus(ctx context.Context, paymentID string) (State, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT to_state FROM payment_events WHERE payment_id = ? ORDER BY seq ASC`, paymentID)
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
		if to != "" {
			st = State(to)
		}
	}
	return st, rows.Err()
}

// --- outbox staging inside the idempotent tx --------------------------------

// stageEventTx inserts one 02 §4.3 envelope into the outbox INSIDE the caller's
// idempotent transaction (idempotency.Execer), so the payment.* event row commits
// atomically with the payment write + the idempotency key (01 §3 / D22): a retried
// money mutation can never produce a second event. Mirrors outbox.SQLStore.WriteInTx.
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

// outboxCountTopic is an audit counter (exactly-once event proof).
func (s *store) outboxCountTopic(ctx context.Context, topic string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE topic = ?`, topic).Scan(&n)
	return n, err
}
