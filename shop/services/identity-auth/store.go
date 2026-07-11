package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one libs/idempotency uses)
)

//go:embed migrations/0001_identity.pg.sql
var pgMigration string

// sqliteMigration is the SQLite twin of the PG schema (types differ only).
const sqliteMigration = `
CREATE TABLE IF NOT EXISTS users (
    user_id    TEXT NOT NULL PRIMARY KEY,
    email      TEXT NOT NULL,
    pw_hash    TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'customer',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_uidx ON users (email);
CREATE TABLE IF NOT EXISTS sessions (
    session_id   TEXT NOT NULL PRIMARY KEY,
    user_id      TEXT NOT NULL,
    refresh_hash TEXT NOT NULL,
    access_jti   TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'customer',
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at   TIMESTAMP NOT NULL,
    revoked      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_refresh_uidx ON sessions (refresh_hash);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id);
`

// Sentinel store errors, mapped to 02 §2 codes by the handlers.
var (
	errEmailTaken   = errors.New("email already registered")
	errNoUser       = errors.New("no such user")
	errSessionState = errors.New("session invalid, revoked, expired, or already rotated")
)

// PGSchema returns the production PostgreSQL migration (for a slice's migration
// runner / CI expand-contract flow) — parity with libs/idempotency.Schema().
func PGSchema() string { return pgMigration }

// store is the identity-auth persistence layer over database/sql. SQLite here
// (file or :memory:), PostgreSQL in prod via the embedded .pg.sql — the Go code
// is engine-agnostic (only "?" placeholders, which modernc/sqlite accepts).
type store struct {
	db *sql.DB
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite: one writer; keep the pool serialized to avoid "database is locked"
	// under the concurrent-login tests. PG has no such limit.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, sqliteMigration); err != nil {
		return nil, fmt.Errorf("identity migrate: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) close() error { return s.db.Close() }

type user struct {
	ID     string
	Email  string
	PWHash string
	Role   string
}

// createUser inserts a new user; returns errEmailTaken on a duplicate email.
func (s *store) createUser(ctx context.Context, u user) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (user_id, email, pw_hash, role) VALUES (?, ?, ?, ?)`,
		u.ID, strings.ToLower(u.Email), u.PWHash, u.Role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return errEmailTaken
		}
		return err
	}
	return nil
}

func (s *store) userByEmail(ctx context.Context, email string) (user, error) {
	var u user
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, email, pw_hash, role FROM users WHERE email = ?`,
		strings.ToLower(email)).Scan(&u.ID, &u.Email, &u.PWHash, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return user{}, errNoUser
	}
	return u, err
}

type session struct {
	ID          string
	UserID      string
	Role        string
	RefreshHash string
	AccessJTI   string
	ExpiresAt   time.Time
}

func (s *store) createSession(ctx context.Context, se session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (session_id, user_id, refresh_hash, access_jti, role, expires_at, revoked)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		se.ID, se.UserID, se.RefreshHash, se.AccessJTI, se.Role, se.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

// rotateSession atomically swaps a session's refresh token (and access jti) for
// new ones, but ONLY if the presented refresh hash is the current one, the
// session is not revoked, and it has not expired. It returns the pre-rotation
// jti (to add to the denylist) and the session/user identity for the new token.
// A failed CAS (wrong/old/reused refresh token) returns errSessionState — the
// signal a stolen or already-rotated token was replayed.
func (s *store) rotateSession(ctx context.Context, oldRefreshHash, newRefreshHash, newJTI string, newExpiry time.Time) (prevJTI string, se session, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", session{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var revoked int
	var expStr string
	err = tx.QueryRowContext(ctx,
		`SELECT session_id, user_id, role, access_jti, revoked, expires_at
		   FROM sessions WHERE refresh_hash = ?`, oldRefreshHash).
		Scan(&se.ID, &se.UserID, &se.Role, &prevJTI, &revoked, &expStr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", session{}, errSessionState
	}
	if err != nil {
		return "", session{}, err
	}
	if revoked != 0 || parseTime(expStr).Before(time.Now()) {
		return "", session{}, errSessionState
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE sessions SET refresh_hash = ?, access_jti = ?, expires_at = ? WHERE session_id = ?`,
		newRefreshHash, newJTI, newExpiry.UTC().Format(time.RFC3339), se.ID); err != nil {
		return "", session{}, err
	}
	if err = tx.Commit(); err != nil {
		return "", session{}, err
	}
	se.AccessJTI = newJTI
	se.RefreshHash = newRefreshHash
	se.ExpiresAt = newExpiry
	return prevJTI, se, nil
}

// revokeByRefresh marks a session revoked given its refresh token hash and
// returns the access jti to add to the denylist.
func (s *store) revokeByRefresh(ctx context.Context, refreshHash string) (jti string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var revoked int
	err = tx.QueryRowContext(ctx,
		`SELECT access_jti, revoked FROM sessions WHERE refresh_hash = ?`, refreshHash).
		Scan(&jti, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSessionState
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE sessions SET revoked = 1 WHERE refresh_hash = ?`, refreshHash); err != nil {
		return "", err
	}
	return jti, tx.Commit()
}

func parseTime(s string) time.Time {
	// Accept both RFC3339 (our writes) and SQLite's default CURRENT_TIMESTAMP.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
