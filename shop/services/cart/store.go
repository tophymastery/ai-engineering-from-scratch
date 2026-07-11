package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one merchant-catalog uses)
)

//go:embed migrations/0001_cart.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with
// merchant-catalog PGSchema()/libs/idempotency Schema()).
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_cart.pg.sql (types only
// differ: TIMESTAMP vs TIMESTAMPTZ, INTEGER vs BIGINT/BOOLEAN). The SQL is
// otherwise engine-agnostic, so the ETag/version concurrency + revalidation
// semantics are identical to production.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS carts (
    cart_id     TEXT NOT NULL PRIMARY KEY,
    user_token  TEXT NOT NULL DEFAULT '',
    currency    TEXT NOT NULL DEFAULT '',
    version     INTEGER NOT NULL DEFAULT 1,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS cart_items (
    cart_id        TEXT NOT NULL,
    item_id        TEXT NOT NULL,
    merchant_id    TEXT NOT NULL,
    name           TEXT NOT NULL,
    unit_amount    INTEGER NOT NULL,
    unit_currency  TEXT NOT NULL,
    quantity       INTEGER NOT NULL,
    available      INTEGER NOT NULL DEFAULT 1,
    menu_version   INTEGER NOT NULL DEFAULT 0,
    revalidated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (cart_id, item_id)
);
CREATE INDEX IF NOT EXISTS cart_items_merchant_idx ON cart_items (merchant_id, item_id);
`

// Sentinel store errors, mapped to 02 §2 codes by the handlers.
var (
	errCartNotFound    = errors.New("no such cart")
	errStaleWrite      = errors.New("stale write: If-Match does not match current ETag")
	errIfMatchRequired = errors.New("If-Match required on a mutating cart edit")
	errItemUnavailable = errors.New("item is not available")
	errItemNotInMenu   = errors.New("item is not on the merchant's menu")
	errMerchantUnknown = errors.New("merchant menu could not be resolved")
	errMixedCurrency   = errors.New("item currency differs from the cart currency")
	errValidation      = errors.New("validation")
)

// money is the 02 §1 Money type: integer minor units + ISO currency; never floats.
type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// addInput is the body of POST /v1/carts/{cart_id}/items.
type addInput struct {
	ItemID     string `json:"item_id"`
	MerchantID string `json:"merchant_id"`
	Quantity   int64  `json:"quantity"`
}

// lineView is one cart line at the API boundary. `available` is the last
// revalidated availability (a menu change can flip it); `revalidated` echoes the
// catalog `menu_version` the price was snapshotted against.
type lineView struct {
	ItemID      string `json:"item_id"`
	MerchantID  string `json:"merchant_id"`
	Name        string `json:"name"`
	UnitPrice   money  `json:"unit_price"`
	Quantity    int64  `json:"quantity"`
	Available   bool   `json:"available"`
	LineTotal   money  `json:"line_total"`
	MenuVersion int64  `json:"menu_version"`
}

// cartView is the assembled cart returned to clients + cached in the Redis-like
// snapshot tier. subtotal sums the AVAILABLE lines only (an item flagged
// unavailable by a menu change is kept in the cart but excluded from the total),
// so a merchant's availability/price change is visible in the subtotal.
type cartView struct {
	CartID   string     `json:"cart_id"`
	Version  int64      `json:"version"`
	ETag     string     `json:"etag"`
	Currency string     `json:"currency"`
	Items    []lineView `json:"items"`
	Subtotal money      `json:"subtotal"`
}

// store is the cart persistence layer (PostgreSQL / SQLite-in-tests) plus its
// Redis-like snapshot tier and its view of the catalog. One PG database per
// service (01 §1); here one in-memory SQLite DB serialised to a single writer
// connection.
type store struct {
	db     *sql.DB
	clock  Clock
	view   *catalogView
	snap   *snapshotStore
	region string
}

func openStore(ctx context.Context, region string, clock Clock, view *catalogView, snap *snapshotStore) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one shared in-memory db, serialised writer
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("cart migrate: %w", err)
	}
	return &store{db: db, clock: clock, view: view, snap: snap, region: region}, nil
}

func (s *store) close() { _ = s.db.Close() }

// cartETag is the strong validator for a cart at a version (02 §1).
func cartETag(cartID string, version int64) string { return makeETag("cart", cartID, version) }

// --- read path (snapshot → PG rehydrate) ---

// getCartView returns the assembled cart. It serves the Redis-like snapshot when
// fresh; on a snapshot miss (absent or past the freshness window) it REHYDRATES
// from PostgreSQL (the durable system of record) and repopulates the snapshot.
// errCartNotFound when the cart does not exist.
func (s *store) getCartView(ctx context.Context, cartID string) (cartView, error) {
	if b, ok := s.snap.get(cartID); ok {
		var v cartView
		if err := json.Unmarshal(b, &v); err == nil {
			return v, nil
		}
		// corrupt snapshot — fall through to a rehydrate
	}
	v, err := s.loadCartView(ctx, cartID)
	if err != nil {
		return cartView{}, err
	}
	s.snap.markRehydrate()
	s.cache(cartID, v)
	return v, nil
}

// loadCartView assembles the cart from PG (no snapshot).
func (s *store) loadCartView(ctx context.Context, cartID string) (cartView, error) {
	var version int64
	var currency string
	err := s.db.QueryRowContext(ctx, `SELECT version, currency FROM carts WHERE cart_id = ?`, cartID).Scan(&version, &currency)
	if err == sql.ErrNoRows {
		return cartView{}, errCartNotFound
	}
	if err != nil {
		return cartView{}, err
	}
	lines, err := s.loadLines(ctx, s.db, cartID)
	if err != nil {
		return cartView{}, err
	}
	return assemble(cartID, version, currency, lines), nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *store) loadLines(ctx context.Context, q queryer, cartID string) ([]lineView, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT item_id, merchant_id, name, unit_amount, unit_currency, quantity, available, menu_version
		   FROM cart_items WHERE cart_id = ? ORDER BY item_id`, cartID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []lineView{}
	for rows.Next() {
		var l lineView
		var avail int
		if err := rows.Scan(&l.ItemID, &l.MerchantID, &l.Name, &l.UnitPrice.Amount, &l.UnitPrice.Currency, &l.Quantity, &avail, &l.MenuVersion); err != nil {
			return nil, err
		}
		l.Available = avail != 0
		l.LineTotal = money{Amount: l.UnitPrice.Amount * l.Quantity, Currency: l.UnitPrice.Currency}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

// assemble builds the cartView + subtotal (available lines only) + ETag.
func assemble(cartID string, version int64, currency string, lines []lineView) cartView {
	var subtotal int64
	for _, l := range lines {
		if l.Available {
			subtotal += l.UnitPrice.Amount * l.Quantity
		}
	}
	return cartView{
		CartID: cartID, Version: version, ETag: cartETag(cartID, version),
		Currency: currency, Items: lines,
		Subtotal: money{Amount: subtotal, Currency: currency},
	}
}

// cache serialises a view into the Redis-like snapshot tier.
func (s *store) cache(cartID string, v cartView) {
	if b, err := json.Marshal(v); err == nil {
		s.snap.set(cartID, b)
	}
}

// --- write path (ETag compare-and-swap in a transaction) ---

// addItem adds a line under optimistic concurrency. The first add to a cart_id
// CREATES the cart (version 1, If-Match not required — the bootstrap). Every
// subsequent add REQUIRES a matching If-Match: an empty header → errIfMatchRequired
// (428), a stale header → errStaleWrite (412). On success the cart version bumps
// by one (new ETag), the snapshot is refreshed, and the fresh cart is returned.
// `priced` is the item's authoritative catalog info (resolved by the caller from
// catalogView / the pact read); an unavailable item is rejected before insert.
func (s *store) addItem(ctx context.Context, cartID, ifMatch string, in addInput, priced itemInfo) (cartView, error) {
	if in.Quantity <= 0 {
		return cartView{}, fmt.Errorf("%w: quantity must be a positive integer", errValidation)
	}
	if !priced.Available {
		return cartView{}, errItemUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cartView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var cur int64
	var currency string
	err = tx.QueryRowContext(ctx, `SELECT version, currency FROM carts WHERE cart_id = ?`, cartID).Scan(&cur, &currency)
	switch {
	case err == sql.ErrNoRows:
		// Bootstrap create: the cart's currency is the first line's currency.
		currency = priced.Currency
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO carts (cart_id, currency, version) VALUES (?, ?, 1)`, cartID, currency); err != nil {
			return cartView{}, err
		}
		cur = 0 // sentinel: created at version 1 below
	case err != nil:
		return cartView{}, err
	default:
		// Existing cart → optimistic-concurrency guard.
		if ifMatch == "" {
			return cartView{}, errIfMatchRequired
		}
		if !etagMatches(ifMatch, cartETag(cartID, cur)) {
			return cartView{}, errStaleWrite
		}
		if currency != "" && priced.Currency != currency {
			return cartView{}, errMixedCurrency
		}
		if currency == "" {
			currency = priced.Currency
		}
	}

	// Upsert the line: add to the existing quantity if already in the cart.
	now := s.clock.Now().UTC()
	res, err := tx.ExecContext(ctx,
		`UPDATE cart_items SET quantity = quantity + ?, name = ?, unit_amount = ?, unit_currency = ?,
		        available = 1, menu_version = ?, revalidated_at = ?
		  WHERE cart_id = ? AND item_id = ?`,
		in.Quantity, priced.Name, priced.Amount, priced.Currency, priced.Version, now, cartID, in.ItemID)
	if err != nil {
		return cartView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cart_items (cart_id, item_id, merchant_id, name, unit_amount, unit_currency, quantity, available, menu_version, revalidated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			cartID, in.ItemID, in.MerchantID, priced.Name, priced.Amount, priced.Currency, in.Quantity, priced.Version, now); err != nil {
			return cartView{}, err
		}
	}

	final, err := s.commitVersion(ctx, tx, cartID, cur, currency)
	if err != nil {
		return cartView{}, err
	}
	return s.finishWrite(ctx, cartID, final)
}

// removeItem removes a line under the same optimistic-concurrency rule (If-Match
// required → 412 on stale). errCartNotFound if the cart does not exist.
func (s *store) removeItem(ctx context.Context, cartID, ifMatch, itemID string) (cartView, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cartView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var cur int64
	var currency string
	err = tx.QueryRowContext(ctx, `SELECT version, currency FROM carts WHERE cart_id = ?`, cartID).Scan(&cur, &currency)
	if err == sql.ErrNoRows {
		return cartView{}, errCartNotFound
	}
	if err != nil {
		return cartView{}, err
	}
	if ifMatch == "" {
		return cartView{}, errIfMatchRequired
	}
	if !etagMatches(ifMatch, cartETag(cartID, cur)) {
		return cartView{}, errStaleWrite
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM cart_items WHERE cart_id = ? AND item_id = ?`, cartID, itemID)
	if err != nil {
		return cartView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return cartView{}, fmt.Errorf("%w: item %s not in cart", errValidation, itemID)
	}
	final, err := s.commitVersion(ctx, tx, cartID, cur, currency)
	if err != nil {
		return cartView{}, err
	}
	return s.finishWrite(ctx, cartID, final)
}

// commitVersion performs the version compare-and-swap and commits. cur==0 means
// the cart was just created (version already 1, no CAS needed). Otherwise it does
// `UPDATE carts SET version=cur+1 WHERE cart_id AND version=cur` — even under PG
// snapshot isolation only the txn that read `cur` unchanged commits the bump; a
// racing txn matches 0 rows → errStaleWrite. Returns the final version.
func (s *store) commitVersion(ctx context.Context, tx *sql.Tx, cartID string, cur int64, currency string) (int64, error) {
	final := int64(1)
	if cur > 0 {
		next := cur + 1
		res, err := tx.ExecContext(ctx,
			`UPDATE carts SET version = ?, currency = ?, updated_at = ? WHERE cart_id = ? AND version = ?`,
			next, currency, s.clock.Now().UTC(), cartID, cur)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, errStaleWrite
		}
		final = next
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return final, nil
}

// finishWrite reloads the fresh cart from PG and refreshes the snapshot.
func (s *store) finishWrite(ctx context.Context, cartID string, _ int64) (cartView, error) {
	v, err := s.loadCartView(ctx, cartID)
	if err != nil {
		return cartView{}, err
	}
	s.cache(cartID, v)
	return v, nil
}

// --- menu-change revalidation (the < 5 s reflection property) ---

// revalidateMerchant reprices/flags every cart line referencing a merchant after
// its menu changed (called by the menu.updated consumer, AFTER catalogView is
// updated). For each affected line it snapshots the new price + availability from
// catalogView (an item no longer on the menu → available=false, last price kept),
// stamps revalidated_at, and — crucially — EAGERLY INVALIDATES the affected cart
// snapshots so the next read rehydrates the repriced state. Reflection is then
// immediate on the next read; the snapshot TTL bounds it to the freshness window
// even if the eager invalidation is missed. Does NOT bump the cart version: a
// revalidation is server-side pricing, not a user edit, so a client's If-Match
// stays valid. Returns the number of cart lines updated.
func (s *store) revalidateMerchant(ctx context.Context, merchantID string, _ int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT cart_id, item_id, unit_amount, unit_currency, name FROM cart_items WHERE merchant_id = ?`, merchantID)
	if err != nil {
		return 0, err
	}
	type target struct {
		cartID, itemID, curCurrency, name string
		curAmount                         int64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.cartID, &t.itemID, &t.curAmount, &t.curCurrency, &t.name); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := s.clock.Now().UTC()
	affected := map[string]struct{}{}
	newVersion := s.view.version(merchantID)
	for _, t := range targets {
		info, itemKnown, _ := s.view.lookup(merchantID, t.itemID)
		amount := t.curAmount
		currency := t.curCurrency
		name := t.name
		available := false
		if itemKnown {
			amount = info.Amount
			currency = info.Currency
			name = info.Name
			available = info.Available
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cart_items SET unit_amount = ?, unit_currency = ?, name = ?, available = ?, menu_version = ?, revalidated_at = ?
			  WHERE cart_id = ? AND item_id = ?`,
			amount, currency, name, boolToInt(available), newVersion, now, t.cartID, t.itemID); err != nil {
			return 0, err
		}
		affected[t.cartID] = struct{}{}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Eager snapshot invalidation → next read rehydrates the repriced cart.
	for cartID := range affected {
		s.snap.invalidate(cartID)
	}
	return len(targets), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// lastRevalidatedAt returns the revalidated_at of a specific line (test/audit —
// used by the freshness proof to time propagation on the frozen clock).
func (s *store) lastRevalidatedAt(ctx context.Context, cartID, itemID string) (time.Time, error) {
	var ts time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT revalidated_at FROM cart_items WHERE cart_id = ? AND item_id = ?`, cartID, itemID).Scan(&ts)
	return ts, err
}
