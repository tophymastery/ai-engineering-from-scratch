package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shop-platform/shop/libs/outbox"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one identity-auth/profile use)
)

//go:embed migrations/0001_catalog.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with
// identity-auth PGSchema()/libs/idempotency Schema()).
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_catalog.pg.sql (types only
// differ: TIMESTAMP vs TIMESTAMPTZ, INTEGER vs BIGINT). The SQL is otherwise
// engine-agnostic, so the ETag/version concurrency and outbox semantics are
// identical to production.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS merchants (
    merchant_id  TEXT NOT NULL PRIMARY KEY,
    name         TEXT NOT NULL,
    region       TEXT NOT NULL DEFAULT 'local',
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS menus (
    merchant_id  TEXT NOT NULL PRIMARY KEY,
    version      INTEGER NOT NULL DEFAULT 1,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS menu_items (
    item_id        TEXT NOT NULL PRIMARY KEY,
    merchant_id    TEXT NOT NULL,
    name           TEXT NOT NULL,
    price_amount   INTEGER NOT NULL,
    price_currency TEXT NOT NULL,
    available      INTEGER NOT NULL DEFAULT 1,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS menu_items_merchant_idx ON menu_items (merchant_id);
CREATE TABLE IF NOT EXISTS store_status (
    merchant_id  TEXT NOT NULL PRIMARY KEY,
    status       TEXT NOT NULL DEFAULT 'CLOSED',
    version      INTEGER NOT NULL DEFAULT 1,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// Sentinel store errors, mapped to 02 §2 codes by the handlers.
var (
	errNoMerchant  = errors.New("no such merchant")
	errDupMerchant = errors.New("merchant already exists")
	errStaleWrite  = errors.New("stale write: If-Match does not match current ETag")
	errBadStatus   = errors.New("invalid store status")
)

var validStatuses = map[string]bool{"OPEN": true, "BUSY": true, "CLOSED": true}

// store is the merchant-catalog persistence layer + its transactional outbox.
// One PG database per service (01 §1); here one in-memory SQLite DB (with the
// outbox tables migrated alongside) serialised to a single writer connection.
type store struct {
	db     *sql.DB
	ob     *outbox.SQLStore
	region string
}

func openStore(ctx context.Context, region string) (*store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one shared in-memory db, serialised writer
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("catalog migrate: %w", err)
	}
	ob := outbox.NewSQLStore(db, outbox.SQLiteDialect{})
	if err := outbox.Migrate(ctx, db, outbox.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("outbox migrate: %w", err)
	}
	return &store{db: db, ob: ob, region: region}, nil
}

func (s *store) close() { _ = s.db.Close() }

// --- domain records at the API boundary ---

type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type itemInput struct {
	ItemID    string `json:"item_id,omitempty"` // set to update an existing item
	Name      string `json:"name"`
	Price     money  `json:"price"`
	Available *bool  `json:"available,omitempty"`
}

type itemView struct {
	ItemID    string `json:"item_id"`
	Name      string `json:"name"`
	Price     money  `json:"price"`
	Available bool   `json:"available"`
}

type menuView struct {
	MerchantID string     `json:"merchant_id"`
	Version    int64      `json:"version"`
	ETag       string     `json:"etag"`
	Items      []itemView `json:"items"`
}

type storeStatusView struct {
	MerchantID string `json:"merchant_id"`
	Status     string `json:"status"`
	Version    int64  `json:"version"`
	ETag       string `json:"etag"`
}

type merchantInput struct {
	MerchantID string `json:"merchant_id,omitempty"`
	Name       string `json:"name"`
}

type merchantView struct {
	MerchantID string          `json:"merchant_id"`
	Name       string          `json:"name"`
	Menu       menuView        `json:"menu"`
	Store      storeStatusView `json:"store_status"`
	CreatedAt  string          `json:"created_at"`
}

// menuPatch is the body of PATCH /v1/merchants/{id}/menu: upsert items (create
// when item_id empty, update when set) and/or remove items by id. Additive,
// typed line-item lists (02 §5).
type menuPatch struct {
	UpsertItems   []itemInput `json:"upsert_items"`
	RemoveItemIDs []string    `json:"remove_item_ids"`
}

// createMerchant bootstraps a merchant with an empty menu (version 1) and a
// CLOSED store status (version 1). merchant_id may be client-supplied (for
// deterministic seeds/pacts) or minted. Publishes an initial menu.updated +
// store.status_changed so consumers learn about the new merchant.
func (s *store) createMerchant(ctx context.Context, ev *eventBuilder, in merchantInput, traceID string) (merchantView, error) {
	mid := in.MerchantID
	if mid == "" {
		mid = newToken("mer")
	}
	name := in.Name
	if name == "" {
		name = "Merchant " + mid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return merchantView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO merchants (merchant_id, name, region) VALUES (?, ?, ?)`, mid, name, s.region); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return merchantView{}, errDupMerchant
		}
		return merchantView{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO menus (merchant_id, version) VALUES (?, 1)`, mid); err != nil {
		return merchantView{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO store_status (merchant_id, status, version) VALUES (?, 'CLOSED', 1)`, mid); err != nil {
		return merchantView{}, err
	}

	menuETag := makeETag("menu", mid, 1)
	statusETag := makeETag("status", mid, 1)
	if err := s.ob.WriteInTx(ctx, tx, topicMenuUpdated,
		ev.menuUpdated(mid, 1, menuETag, nil, traceID)); err != nil {
		return merchantView{}, err
	}
	if err := s.ob.WriteInTx(ctx, tx, topicStoreStatus,
		ev.storeStatusChanged(mid, "CLOSED", 1, statusETag, traceID)); err != nil {
		return merchantView{}, err
	}
	if err := tx.Commit(); err != nil {
		return merchantView{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	return merchantView{
		MerchantID: mid, Name: name, CreatedAt: now,
		Menu:  menuView{MerchantID: mid, Version: 1, ETag: menuETag, Items: []itemView{}},
		Store: storeStatusView{MerchantID: mid, Status: "CLOSED", Version: 1, ETag: statusETag},
	}, nil
}

// getMenu reads the current menu + its ETag. errNoMerchant when absent.
func (s *store) getMenu(ctx context.Context, merchantID string) (menuView, error) {
	var version int64
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM menus WHERE merchant_id = ?`, merchantID).Scan(&version)
	if err == sql.ErrNoRows {
		return menuView{}, errNoMerchant
	}
	if err != nil {
		return menuView{}, err
	}
	items, err := s.loadItems(ctx, s.db, merchantID)
	if err != nil {
		return menuView{}, err
	}
	return menuView{
		MerchantID: merchantID, Version: version,
		ETag: makeETag("menu", merchantID, version), Items: items,
	}, nil
}

func (s *store) loadItems(ctx context.Context, q queryer, merchantID string) ([]itemView, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT item_id, name, price_amount, price_currency, available
		   FROM menu_items WHERE merchant_id = ? ORDER BY item_id`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []itemView{}
	for rows.Next() {
		var it itemView
		var avail int
		if err := rows.Scan(&it.ItemID, &it.Name, &it.Price.Amount, &it.Price.Currency, &avail); err != nil {
			return nil, err
		}
		it.Available = avail != 0
		items = append(items, it)
	}
	return items, rows.Err()
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// patchMenu applies a menu edit under optimistic concurrency. ifMatch MUST equal
// the current menu ETag or the write is rejected with errStaleWrite (→ 412). On
// success the menu version bumps by one, a menu.updated event carrying the full
// new snapshot is written to the outbox IN THE SAME TRANSACTION (exactly-once),
// and the fresh menu + ETag are returned.
func (s *store) patchMenu(ctx context.Context, ev *eventBuilder, merchantID, ifMatch string, patch menuPatch, traceID string) (menuView, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return menuView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var cur int64
	err = tx.QueryRowContext(ctx, `SELECT version FROM menus WHERE merchant_id = ?`, merchantID).Scan(&cur)
	if err == sql.ErrNoRows {
		return menuView{}, errNoMerchant
	}
	if err != nil {
		return menuView{}, err
	}
	// Stale-write guard (02 §1): the client's If-Match must equal the CURRENT
	// menu ETag. A concurrent writer that already bumped the version changed the
	// ETag, so this comparison fails and we reject with 412.
	if !etagMatches(ifMatch, makeETag("menu", merchantID, cur)) {
		return menuView{}, errStaleWrite
	}

	// Validate + apply item changes.
	for _, it := range patch.UpsertItems {
		if it.ItemID == "" {
			id := newToken("itm")
			avail := true
			if it.Available != nil {
				avail = *it.Available
			}
			if it.Name == "" || it.Price.Currency == "" {
				return menuView{}, fmt.Errorf("%w: item name and price.currency are required", errValidation)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO menu_items (item_id, merchant_id, name, price_amount, price_currency, available)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				id, merchantID, it.Name, it.Price.Amount, it.Price.Currency, boolToInt(avail)); err != nil {
				return menuView{}, err
			}
			continue
		}
		// Update existing item (only fields present change; here full replace of
		// the mutable fields for simplicity — name/price/available).
		res, err := tx.ExecContext(ctx,
			`UPDATE menu_items SET name = COALESCE(NULLIF(?, ''), name),
			        price_amount = ?, price_currency = COALESCE(NULLIF(?, ''), price_currency),
			        available = ?
			  WHERE item_id = ? AND merchant_id = ?`,
			it.Name, it.Price.Amount, it.Price.Currency, boolToInt(availOrTrue(it.Available)),
			it.ItemID, merchantID)
		if err != nil {
			return menuView{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return menuView{}, fmt.Errorf("%w: item %s not found for merchant", errValidation, it.ItemID)
		}
	}
	for _, id := range patch.RemoveItemIDs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM menu_items WHERE item_id = ? AND merchant_id = ?`, id, merchantID); err != nil {
			return menuView{}, err
		}
	}

	// Compare-and-swap the version (belt-and-suspenders atomic guard: even under
	// PG snapshot isolation, only the txn that reads `cur` and finds it unchanged
	// commits the bump; a racing txn's WHERE version=cur matches 0 rows).
	next := cur + 1
	res, err := tx.ExecContext(ctx,
		`UPDATE menus SET version = ?, updated_at = ? WHERE merchant_id = ? AND version = ?`,
		next, time.Now().UTC(), merchantID, cur)
	if err != nil {
		return menuView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return menuView{}, errStaleWrite
	}

	items, err := s.loadItems(ctx, tx, merchantID)
	if err != nil {
		return menuView{}, err
	}
	etag := makeETag("menu", merchantID, next)
	if err := s.ob.WriteInTx(ctx, tx, topicMenuUpdated,
		ev.menuUpdated(merchantID, next, etag, toSnapshots(items), traceID)); err != nil {
		return menuView{}, err
	}
	if err := tx.Commit(); err != nil {
		return menuView{}, err
	}
	return menuView{MerchantID: merchantID, Version: next, ETag: etag, Items: items}, nil
}

// getStoreStatus reads the current store status + its ETag.
func (s *store) getStoreStatus(ctx context.Context, merchantID string) (storeStatusView, error) {
	var status string
	var version int64
	err := s.db.QueryRowContext(ctx,
		`SELECT status, version FROM store_status WHERE merchant_id = ?`, merchantID).Scan(&status, &version)
	if err == sql.ErrNoRows {
		return storeStatusView{}, errNoMerchant
	}
	if err != nil {
		return storeStatusView{}, err
	}
	return storeStatusView{
		MerchantID: merchantID, Status: status, Version: version,
		ETag: makeETag("status", merchantID, version),
	}, nil
}

// putStoreStatus sets the store status under optimistic concurrency (same 412
// rule as patchMenu) and publishes store.status_changed transactionally.
func (s *store) putStoreStatus(ctx context.Context, ev *eventBuilder, merchantID, ifMatch, newStatus, traceID string) (storeStatusView, error) {
	if !validStatuses[newStatus] {
		return storeStatusView{}, errBadStatus
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storeStatusView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var cur int64
	err = tx.QueryRowContext(ctx, `SELECT version FROM store_status WHERE merchant_id = ?`, merchantID).Scan(&cur)
	if err == sql.ErrNoRows {
		return storeStatusView{}, errNoMerchant
	}
	if err != nil {
		return storeStatusView{}, err
	}
	if !etagMatches(ifMatch, makeETag("status", merchantID, cur)) {
		return storeStatusView{}, errStaleWrite
	}
	next := cur + 1
	res, err := tx.ExecContext(ctx,
		`UPDATE store_status SET status = ?, version = ?, updated_at = ? WHERE merchant_id = ? AND version = ?`,
		newStatus, next, time.Now().UTC(), merchantID, cur)
	if err != nil {
		return storeStatusView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storeStatusView{}, errStaleWrite
	}
	etag := makeETag("status", merchantID, next)
	if err := s.ob.WriteInTx(ctx, tx, topicStoreStatus,
		ev.storeStatusChanged(merchantID, newStatus, next, etag, traceID)); err != nil {
		return storeStatusView{}, err
	}
	if err := tx.Commit(); err != nil {
		return storeStatusView{}, err
	}
	return storeStatusView{MerchantID: merchantID, Status: newStatus, Version: next, ETag: etag}, nil
}

func toSnapshots(items []itemView) []itemSnapshot {
	out := make([]itemSnapshot, 0, len(items))
	for _, it := range items {
		out = append(out, itemSnapshot{
			ItemID: it.ItemID, Name: it.Name,
			Amount: it.Price.Amount, Currency: it.Price.Currency, Available: it.Available,
		})
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func availOrTrue(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}
