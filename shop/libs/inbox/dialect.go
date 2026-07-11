package inbox

import (
	"fmt"
	"strings"
)

// Dialect abstracts engine SQL differences (same pattern as libs/idempotency
// and libs/outbox). PG in production; SQLite in tests.
type Dialect interface {
	Placeholder(n int) string
	IsUniqueViolation(err error) bool
	Name() string
}

// PostgresDialect implements Dialect for PostgreSQL.
type PostgresDialect struct{}

func (PostgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (PostgresDialect) Name() string             { return "postgres" }
func (PostgresDialect) IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "23505") ||
		strings.Contains(s, "duplicate key value") ||
		strings.Contains(s, "unique constraint")
}

// SQLiteDialect implements Dialect for SQLite (modernc.org/sqlite).
type SQLiteDialect struct{}

func (SQLiteDialect) Placeholder(int) string { return "?" }
func (SQLiteDialect) Name() string           { return "sqlite" }
func (SQLiteDialect) IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "constraint failed: UNIQUE") ||
		strings.Contains(s, "(2067)") ||
		strings.Contains(s, "(1555)")
}

func phList(d Dialect, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = d.Placeholder(i + 1)
	}
	return strings.Join(parts, ", ")
}
