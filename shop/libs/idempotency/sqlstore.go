package idempotency

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SQLStore is the D9 durable store over database/sql — engine-agnostic via a
// Dialect. The idempotency key lives in one table with UNIQUE(idempotency_key);
// the insert, the business write, and the response persistence share a single
// *sql.Tx, so they commit atomically (effect-once). This is the production path
// (PostgreSQL); the same code runs on SQLite in tests.
type SQLStore struct {
	db      *sql.DB
	dialect Dialect
	table   string
}

// NewSQLStore builds a store over db with the given dialect. Table defaults to
// "idempotency_keys".
func NewSQLStore(db *sql.DB, dialect Dialect) *SQLStore {
	return &SQLStore{db: db, dialect: dialect, table: "idempotency_keys"}
}

// Placeholder exposes the dialect's bind marker for portable business callers.
func (s *SQLStore) Placeholder(n int) string { return s.dialect.Placeholder(n) }

// Dialect returns the store's dialect (diagnostics).
func (s *SQLStore) Dialect() Dialect { return s.dialect }

// Begin opens a durable unit of work. The first statement in the returned
// UnitOfWork is always the key INSERT (a write), so SQLite acquires its write
// lock immediately — avoiding read→write upgrade deadlocks.
func (s *SQLStore) Begin(ctx context.Context) (UnitOfWork, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlUoW{store: s, tx: tx}, nil
}

// Get reads a committed record on a fresh connection (loser replay path).
func (s *SQLStore) Get(ctx context.Context, key string) (Record, bool, error) {
	q := fmt.Sprintf(
		`SELECT idempotency_key, request_hash, status, response_code, response_body, created_at
		   FROM %s WHERE idempotency_key = %s`, s.table, s.dialect.Placeholder(1))
	var (
		rec       Record
		code      sql.NullInt64
		body      []byte
		status    string
		createdAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, q, key).Scan(&rec.Key, &rec.ReqHash, &status, &code, &body, &createdAt)
	if err == sql.ErrNoRows {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	rec.Status = Status(status)
	rec.Code = int(code.Int64)
	rec.Body = body
	if createdAt.Valid {
		rec.CreatedAt = createdAt.Time
	}
	return rec, true, nil
}

// sqlUoW is a single durable transaction.
type sqlUoW struct {
	store *SQLStore
	tx    *sql.Tx
}

func (u *sqlUoW) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := u.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (u *sqlUoW) Insert(ctx context.Context, key, reqHash string) error {
	d := u.store.dialect
	q := fmt.Sprintf(
		`INSERT INTO %s (idempotency_key, request_hash, status, created_at)
		 VALUES (%s, %s, %s, %s)`,
		u.store.table, d.Placeholder(1), d.Placeholder(2), d.Placeholder(3), d.Placeholder(4))
	_, err := u.tx.ExecContext(ctx, q, key, reqHash, string(StatusInFlight), time.Now().UTC())
	if err != nil {
		if d.IsUniqueViolation(err) {
			return fmt.Errorf("%w: %v", errKeyConflict, err)
		}
		return err
	}
	return nil
}

func (u *sqlUoW) SaveResponse(ctx context.Context, key string, code int, body []byte) error {
	d := u.store.dialect
	q := fmt.Sprintf(
		`UPDATE %s SET status = %s, response_code = %s, response_body = %s
		  WHERE idempotency_key = %s`,
		u.store.table, d.Placeholder(1), d.Placeholder(2), d.Placeholder(3), d.Placeholder(4))
	_, err := u.tx.ExecContext(ctx, q, string(StatusDone), code, body, key)
	return err
}

func (u *sqlUoW) Commit() error   { return u.tx.Commit() }
func (u *sqlUoW) Rollback() error { return u.tx.Rollback() }
