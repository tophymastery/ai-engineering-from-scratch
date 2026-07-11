package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one cart/merchant-catalog use)
)

//go:embed migrations/0001_pricing.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with cart's
// PGSchema()/libs/idempotency Schema()). It is the checkout-persistence schema —
// the `quotes` table is written ONLY at checkout (D10).
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_pricing.pg.sql (types only
// differ: TIMESTAMPTZ→TIMESTAMP, BIGINT→INTEGER). The persistence semantics —
// one row per checked-out quote, written only at checkout — are identical.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS quotes (
    quote_id       TEXT NOT NULL PRIMARY KEY,
    cart_id        TEXT NOT NULL,
    currency       TEXT NOT NULL,
    subtotal_minor INTEGER NOT NULL,
    total_minor    INTEGER NOT NULL,
    fees_json      TEXT NOT NULL DEFAULT '[]',
    discounts_json TEXT NOT NULL DEFAULT '[]',
    kid            TEXT NOT NULL,
    signature      TEXT NOT NULL,
    issued_at      TIMESTAMP NOT NULL,
    expires_at     TIMESTAMP NOT NULL,
    checked_out_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// --- Redis-like TTL quote tier ---------------------------------------------
//
// D10: "Quotes (10-min TTL, ~99% never used) live in Redis, signed … persisted
// to PG only at checkout." No Redis daemon in this sandbox (disclosed in
// VERIFICATION.md §V-T8), so quoteCache is the in-process stand-in — the SAME
// fresh/miss TTL contract a Redis `SET quote:<id> <json> EX 600` gives, read
// under the injected Clock. It is the SOLE store a POST /v1/quotes writes to; the
// PG `quotes` table is untouched until checkout. Reuses the V-T6/V-T7 snapshot
// TTL shape (a concurrent TTL map under an injected Clock).

type quoteEntry struct {
	val      []byte
	storedAt time.Time
}

type quoteCache struct {
	clock Clock
	ttl   time.Duration

	mu   sync.RWMutex
	data map[string]quoteEntry

	sets   int64
	hits   int64
	misses int64
}

func newQuoteCache(clock Clock, ttl time.Duration) *quoteCache {
	if clock == nil {
		clock = SystemClock{}
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &quoteCache{clock: clock, ttl: ttl, data: map[string]quoteEntry{}}
}

// put stores a signed quote (its JSON) under its 10-min TTL.
func (c *quoteCache) put(q *Quote) error {
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	now := c.clock.Now()
	c.mu.Lock()
	c.data[q.QuoteID] = quoteEntry{val: append([]byte(nil), b...), storedAt: now}
	c.sets++
	c.mu.Unlock()
	return nil
}

// get returns the cached quote when present AND within the freshness window. A
// miss (absent or past TTL — a Redis EX expiry) returns ok=false.
func (c *quoteCache) get(quoteID string) (*Quote, bool) {
	now := c.clock.Now()
	c.mu.Lock()
	e, ok := c.data[quoteID]
	if !ok || now.Sub(e.storedAt) >= c.ttl {
		c.misses++
		c.mu.Unlock()
		return nil, false
	}
	c.hits++
	c.mu.Unlock()
	var q Quote
	if err := json.Unmarshal(e.val, &q); err != nil {
		return nil, false
	}
	return &q, true
}

func (c *quoteCache) stats() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]int64{"sets": c.sets, "hits": c.hits, "misses": c.misses, "entries": int64(len(c.data))}
}

// --- PG (SQLite in tests) checkout persistence -----------------------------

type store struct {
	db     *sql.DB
	clock  Clock
	region string
}

func openStore(ctx context.Context, region string, clock Clock) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one shared in-memory db, serialised writer
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("pricing migrate: %w", err)
	}
	return &store{db: db, clock: clock, region: region}, nil
}

func (s *store) close() { _ = s.db.Close() }

// persistAtCheckout writes exactly one durable row for a quote being consumed at
// checkout. This is the ONLY place the `quotes` table is written (D10 / V-T8
// property #3 — asserted by TestPGWritesOnlyAtCheckout: POST /v1/quotes produces
// 0 rows, checkout produces 1). Idempotent on quote_id: a re-submitted checkout
// (double-tap / retry) does NOT create a second row.
func (s *store) persistAtCheckout(ctx context.Context, q *Quote) error {
	feesJSON, _ := json.Marshal(q.Fees)
	discJSON, _ := json.Marshal(q.Discounts)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO quotes (quote_id, cart_id, currency, subtotal_minor, total_minor,
		     fees_json, discounts_json, kid, signature, issued_at, expires_at, checked_out_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(quote_id) DO NOTHING`,
		q.QuoteID, q.CartID, q.Currency, q.Subtotal.Amount, q.Total.Amount,
		string(feesJSON), string(discJSON), q.Kid, q.Signature,
		q.IssuedAt, q.ExpiresAt, s.clock.Now().UTC())
	return err
}

// quoteRowCount returns the number of persisted (checked-out) quotes — the
// row-count assertion the PG-only-at-checkout integration test reads.
func (s *store) quoteRowCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quotes`).Scan(&n)
	return n, err
}

// checkedOut reports whether a specific quote_id has been persisted.
func (s *store) checkedOut(ctx context.Context, quoteID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quotes WHERE quote_id = ?`, quoteID).Scan(&n)
	return n > 0, err
}
