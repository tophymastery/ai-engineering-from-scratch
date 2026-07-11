package idempotency

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

// pgMigration is the production PostgreSQL DDL, embedded so adopting slices can
// apply it programmatically (or read Schema() to feed their own migration tool).
//
//go:embed migrations/0001_idempotency.pg.sql
var pgMigration string

// sqliteMigration is the equivalent DDL for SQLite (test engine). Same table and
// UNIQUE(idempotency_key); only column types differ (BLOB/TEXT for BYTEA/TIMESTAMPTZ).
const sqliteMigration = `
CREATE TABLE IF NOT EXISTS idempotency_keys (
    idempotency_key TEXT    NOT NULL PRIMARY KEY,
    request_hash    TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'IN_FLIGHT',
    response_code   INTEGER,
    response_body   BLOB,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idempotency_keys_created_at_idx ON idempotency_keys (created_at);
`

// Schema returns the production PostgreSQL migration SQL (for slices wiring it
// into their own migration runner / CI expand-contract flow, 04 §1.3).
func Schema() string { return pgMigration }

// Migrate applies the idempotency table for the given dialect. It is the
// "migration helper" adopting slices call in tests or bootstrap; production
// slices should prefer their versioned migration runner fed by Schema().
func Migrate(ctx context.Context, db *sql.DB, dialect Dialect) error {
	var ddl string
	switch dialect.Name() {
	case "postgres":
		ddl = pgMigration
	case "sqlite":
		ddl = sqliteMigration
	default:
		return fmt.Errorf("idempotency: no migration for dialect %q", dialect.Name())
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("idempotency: migrate (%s): %w", dialect.Name(), err)
	}
	return nil
}
