// Package idempotency implements D9: effect-once via a durable
// UNIQUE(idempotency_key) insert executed INSIDE the caller's own DB
// transaction, in the same commit as the business write. A pluggable cache
// (in-memory here, Redis in prod) holds a read-through response copy and an
// IN_FLIGHT advisory marker ONLY — losing it degrades latency, never
// correctness. The 02 §3 wire protocol (headers, replay semantics) is unchanged.
//
// Core contract (D9): "BeginTx → insert key → business write → store response →
// commit". Under N concurrent same-key requests exactly one transaction wins
// the unique insert and runs the business effect; the losers block on the
// unique index, observe the winner's committed row, and replay its stored
// response. A unique constraint cannot lose an acked write on failover, which
// is why it — not Redis SETNX — is the source of truth.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

// Status of a stored idempotency record. A durably-committed record is always
// DONE (the single-txn write goes straight to DONE at commit); IN_FLIGHT is an
// advisory cache-only marker for fast double-tap rejection.
type Status string

const (
	StatusInFlight Status = "IN_FLIGHT"
	StatusDone     Status = "DONE"
)

// Record is a stored idempotency entry: the key, the request hash that produced
// it, and the response to replay.
type Record struct {
	Key       string
	ReqHash   string
	Status    Status
	Code      int
	Body      []byte
	CreatedAt time.Time
}

// Outcome is the result of Do.
type Outcome struct {
	Code     int
	Body     []byte
	Replayed bool // true when this response was served from a prior committed effect
}

// sentinel errors used internally between Store and Manager.
var (
	// errKeyConflict is returned by a UnitOfWork.Insert when the key already
	// exists (unique violation): the caller must roll back and re-read.
	errKeyConflict = errors.New("idempotency: key already exists")
)

// Execer is the subset of a transaction the business write uses. The business
// write MUST run through this so it commits atomically with the idempotency key
// (D9). SQLStore backs it with *sql.Tx; MemStore backs it with a staged op list.
type Execer interface {
	// Exec runs one write inside the idempotent transaction and returns rows
	// affected. Placeholders follow the store's dialect ($1.. for pg, ? for
	// sqlite); use the store's Placeholder helper for portable callers.
	Exec(ctx context.Context, query string, args ...any) (int64, error)
}

// UnitOfWork is one durable transaction: the key insert, the business write, and
// the response persistence commit together or not at all.
type UnitOfWork interface {
	Execer
	// Insert attempts the UNIQUE(idempotency_key) insert. Returns nil on win.
	// Returns errKeyConflict (wrapped) if the key already exists.
	Insert(ctx context.Context, key, reqHash string) error
	// SaveResponse persists the final response for key within this transaction.
	SaveResponse(ctx context.Context, key string, code int, body []byte) error
	Commit() error
	Rollback() error
}

// Store opens durable units of work and supports a standalone read for the
// loser replay path.
type Store interface {
	Begin(ctx context.Context) (UnitOfWork, error)
	// Get reads a committed record outside any transaction (fresh connection),
	// used by the loser after a unique violation. ok=false if absent.
	Get(ctx context.Context, key string) (Record, bool, error)
}

// BusinessFunc runs the caller's effect inside the idempotent transaction and
// returns the HTTP-shaped response to persist and replay. It receives the same
// UnitOfWork so its writes are atomic with the key (D9).
type BusinessFunc func(ctx context.Context, tx Execer) (code int, body []byte, err error)

// Manager wires a durable Store with an advisory Cache.
type Manager struct {
	store Store
	cache Cache
	// MaxReadRetries bounds the loser re-read loop (should resolve on the first
	// try since the winner commits key+response atomically).
	MaxReadRetries int
	// InProgressRetryAfter is advised to clients on a rare IN_FLIGHT advisory hit.
	InProgressRetryAfter time.Duration
}

// New builds a Manager. cache may be nil (pure-DB mode: correct but uncached).
func New(store Store, cache Cache) *Manager {
	return &Manager{
		store:                store,
		cache:                cache,
		MaxReadRetries:       50,
		InProgressRetryAfter: 100 * time.Millisecond,
	}
}

// Do executes business exactly once for (key, reqHash), durably. It returns an
// Outcome (fresh or replayed) or a *shoperr.Error (IDEMPOTENCY_KEY_REUSED /
// IDEMPOTENCY_IN_PROGRESS) mapped to the 02 §3 wire by the HTTP helper.
func (m *Manager) Do(ctx context.Context, key, reqHash string, business BusinessFunc) (Outcome, error) {
	// 1. Advisory cache fast path (read-through). Correctness never depends on
	//    this; it just avoids a DB round-trip for hot replays.
	if m.cache != nil {
		if rec, ok := m.cache.Get(ctx, key); ok && rec.Status == StatusDone {
			return m.classifyExisting(rec, reqHash)
		}
	}

	// 2. Durable path: BeginTx → insert key → business → save response → commit.
	uow, err := m.store.Begin(ctx)
	if err != nil {
		return Outcome{}, err
	}
	insErr := uow.Insert(ctx, key, reqHash)
	if insErr == nil {
		// We won the race: run the business effect in THIS transaction.
		code, body, bizErr := business(ctx, uow)
		if bizErr != nil {
			_ = uow.Rollback()
			return Outcome{}, bizErr
		}
		if err := uow.SaveResponse(ctx, key, code, body); err != nil {
			_ = uow.Rollback()
			return Outcome{}, err
		}
		if err := uow.Commit(); err != nil {
			_ = uow.Rollback()
			return Outcome{}, err
		}
		if m.cache != nil {
			m.cache.Set(ctx, key, Record{Key: key, ReqHash: reqHash, Status: StatusDone, Code: code, Body: body})
		}
		return Outcome{Code: code, Body: body, Replayed: false}, nil
	}
	// Not a conflict ⇒ a real failure.
	if !errors.Is(insErr, errKeyConflict) {
		_ = uow.Rollback()
		return Outcome{}, insErr
	}
	// 3. We lost the race: roll back and read the winner's committed record.
	_ = uow.Rollback()
	rec, err := m.readCommitted(ctx, key)
	if err != nil {
		return Outcome{}, err
	}
	if m.cache != nil {
		m.cache.Set(ctx, key, rec)
	}
	return m.classifyExisting(rec, reqHash)
}

// readCommitted reads the committed record for a lost race, retrying briefly in
// the (theoretically impossible with single-txn writers) event the response row
// is not yet visible.
func (m *Manager) readCommitted(ctx context.Context, key string) (Record, error) {
	backoff := time.Millisecond
	for i := 0; i <= m.MaxReadRetries; i++ {
		rec, ok, err := m.store.Get(ctx, key)
		if err != nil {
			return Record{}, err
		}
		if ok && rec.Status == StatusDone {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 20*time.Millisecond {
			backoff *= 2
		}
	}
	return Record{}, shoperr.New(shoperr.CodeIdempotencyInProgress, "")
}

// classifyExisting turns a committed record into a replay Outcome or a 409.
func (m *Manager) classifyExisting(rec Record, reqHash string) (Outcome, error) {
	if rec.ReqHash == reqHash {
		return Outcome{Code: rec.Code, Body: rec.Body, Replayed: true}, nil
	}
	// Same key, different request body ⇒ 409 IDEMPOTENCY_KEY_REUSED (02 §3).
	return Outcome{}, shoperr.New(shoperr.CodeIdempotencyKeyReuse, "")
}

// RequestHash computes the canonical request hash used to detect same-key/
// different-body reuse: sha256 over method, path and the exact request body.
func RequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
