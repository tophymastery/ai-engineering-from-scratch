package inbox

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed migrations/0001_inbox.pg.sql
var pgMigration string

// sqliteMigration mirrors the PG schema for tests (no native partitioning;
// part_day is a plain column and drops are DELETE-by-day).
const sqliteMigration = `
CREATE TABLE IF NOT EXISTS inbox (
    event_id       TEXT NOT NULL,
    consumer_group TEXT NOT NULL,
    part_day       TEXT NOT NULL,
    processed_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (consumer_group, event_id)
);
CREATE INDEX IF NOT EXISTS inbox_part_day_idx ON inbox (part_day);
CREATE TABLE IF NOT EXISTS dlq (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id       TEXT NOT NULL,
    consumer_group TEXT NOT NULL,
    topic          TEXT NOT NULL,
    agg_key        TEXT NOT NULL,
    payload        BLOB NOT NULL,
    attempts       INTEGER NOT NULL,
    cause          TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'parked',
    part_day       TEXT NOT NULL,
    parked_at      TIMESTAMP NOT NULL,
    replayed_at    TIMESTAMP
);
CREATE INDEX IF NOT EXISTS dlq_group_status_idx ON dlq (consumer_group, status);
`

// Schema returns the production PostgreSQL migration.
func Schema() string { return pgMigration }

// Migrate applies the inbox + dlq tables for the given dialect.
func Migrate(ctx context.Context, db *sql.DB, d Dialect) error {
	var ddl string
	switch d.Name() {
	case "postgres":
		ddl = pgMigration
	case "sqlite":
		ddl = sqliteMigration
	default:
		return fmt.Errorf("inbox: no migration for dialect %q", d.Name())
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("inbox: migrate (%s): %w", d.Name(), err)
	}
	return nil
}
