package outbox

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed migrations/0001_outbox.pg.sql
var pgMigration string

// sqliteMigration is the SQLite equivalent for tests. SQLite has no native
// range partitioning, so part_day is a plain column and "partition drop" is a
// DELETE by part_day (DropPublishedBefore); the guard makes it loss-free.
const sqliteMigration = `
CREATE TABLE IF NOT EXISTS outbox (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id    TEXT    NOT NULL,
    topic       TEXT    NOT NULL,
    agg_key     TEXT    NOT NULL,
    payload     BLOB    NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    part_day    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS outbox_id_idx ON outbox (id);
CREATE INDEX IF NOT EXISTS outbox_part_day_idx ON outbox (part_day);
CREATE TABLE IF NOT EXISTS outbox_relay_cursor (
    relay_name TEXT NOT NULL PRIMARY KEY,
    last_id    INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL
);
`

// Schema returns the production PostgreSQL migration (for a slice's own runner).
func Schema() string { return pgMigration }

// Migrate applies the outbox + cursor tables for the given dialect.
func Migrate(ctx context.Context, db *sql.DB, d Dialect) error {
	var ddl string
	switch d.Name() {
	case "postgres":
		ddl = pgMigration
	case "sqlite":
		ddl = sqliteMigration
	default:
		return fmt.Errorf("outbox: no migration for dialect %q", d.Name())
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("outbox: migrate (%s): %w", d.Name(), err)
	}
	return nil
}
