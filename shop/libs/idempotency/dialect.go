package idempotency

import (
	"fmt"
	"strings"
)

// Dialect abstracts the small set of SQL differences between engines so the
// SQLStore itself is engine-agnostic over database/sql. Production uses
// Postgres (D9 = PG-durable); tests also run SQLite (pure-Go, identical
// UNIQUE-constraint-in-transaction semantics) so the concurrency criteria run
// without a server when needed.
type Dialect interface {
	// Placeholder renders the bind marker for the n-th arg (1-based): "$1" (pg)
	// or "?" (sqlite/mysql).
	Placeholder(n int) string
	// IsUniqueViolation reports whether err is a UNIQUE(idempotency_key)
	// conflict — the signal that another transaction won the race.
	IsUniqueViolation(err error) bool
	// Name is a short identifier for logs/VERIFICATION.
	Name() string
}

// PostgresDialect implements Dialect for PostgreSQL (lib/pq or pgx).
type PostgresDialect struct{}

func (PostgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (PostgresDialect) Name() string             { return "postgres" }
func (PostgresDialect) IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// pq/pgx surface SQLSTATE 23505; match on code or message so the core needs
	// no driver-specific import.
	return strings.Contains(s, "23505") ||
		strings.Contains(s, "duplicate key value") ||
		strings.Contains(s, "unique constraint")
}

// SQLiteDialect implements Dialect for SQLite (modernc.org/sqlite, mattn).
type SQLiteDialect struct{}

func (SQLiteDialect) Placeholder(n int) string { return "?" }
func (SQLiteDialect) Name() string             { return "sqlite" }
func (SQLiteDialect) IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// SQLITE_CONSTRAINT_UNIQUE / _PRIMARYKEY.
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "constraint failed: UNIQUE") ||
		strings.Contains(s, "(2067)") || // SQLITE_CONSTRAINT_UNIQUE extended code
		strings.Contains(s, "(1555)") // SQLITE_CONSTRAINT_PRIMARYKEY
}
