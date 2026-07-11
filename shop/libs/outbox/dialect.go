package outbox

import (
	"fmt"
	"strings"
)

// Dialect abstracts the small SQL differences between engines so the store is
// engine-agnostic over database/sql (same pattern as libs/idempotency). PG is
// production; SQLite runs the tests without a server.
type Dialect interface {
	Placeholder(n int) string
	Name() string
}

// PostgresDialect implements Dialect for PostgreSQL.
type PostgresDialect struct{}

func (PostgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (PostgresDialect) Name() string             { return "postgres" }

// SQLiteDialect implements Dialect for SQLite (modernc.org/sqlite).
type SQLiteDialect struct{}

func (SQLiteDialect) Placeholder(int) string { return "?" }
func (SQLiteDialect) Name() string           { return "sqlite" }

// ph renders a list of placeholders "$1,$2,..." / "?,?,..." for readability.
func ph(d Dialect, n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = d.Placeholder(i + 1)
	}
	return strings.Join(parts, ", ")
}
