package idempotency

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemStore is a transactional in-memory Store implementing the SAME effect-once
// semantics as SQLStore via a UNIQUE-violation SIMULATION: a per-key gate makes
// concurrent same-key transactions serialise exactly as a DB blocks losers on a
// unique index — the winner reserves the key, losers wait for it to resolve,
// then observe the committed record and replay. It stands in for a real
// database/sql engine where none is available (the DB-adaptation fallback) and
// backs the reference service (no server needed at runtime).
//
// Scope note: the idempotency KEY lifecycle is fully transactional here
// (reserve → commit/rollback). Arbitrary business-SQL atomicity is the
// SQLStore's job; MemStore.Exec stages closures applied on commit / discarded
// on rollback, so a MemStore effect is also committed atomically with its key.
type MemStore struct {
	mu        sync.Mutex
	committed map[string]Record
	gates     map[string]*keyGate // keys currently reserved by an open txn
}

// keyGate lets losers block until the reserving transaction resolves.
type keyGate struct{ done chan struct{} }

// NewMemStore builds an empty transactional in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{committed: map[string]Record{}, gates: map[string]*keyGate{}}
}

// Begin opens a unit of work.
func (m *MemStore) Begin(_ context.Context) (UnitOfWork, error) {
	return &memUoW{store: m}, nil
}

// Get reads a committed record.
func (m *MemStore) Get(_ context.Context, key string) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.committed[key]
	return r, ok, nil
}

// reserve attempts to reserve key for the calling txn. Returns nil on win; on a
// concurrent/committed conflict it waits for the holder to resolve and returns
// errKeyConflict once the key is durably committed.
func (m *MemStore) reserve(ctx context.Context, key string) error {
	for {
		m.mu.Lock()
		if _, done := m.committed[key]; done {
			m.mu.Unlock()
			return fmt.Errorf("%w: committed", errKeyConflict)
		}
		if g, held := m.gates[key]; held {
			// Another txn holds the key: block until it resolves, then re-check.
			m.mu.Unlock()
			select {
			case <-g.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		// We win: reserve.
		m.gates[key] = &keyGate{done: make(chan struct{})}
		m.mu.Unlock()
		return nil
	}
}

// resolve commits or releases a reserved key and wakes any waiters.
func (m *MemStore) resolve(key string, rec *Record) {
	m.mu.Lock()
	g := m.gates[key]
	if rec != nil {
		m.committed[key] = *rec
	}
	delete(m.gates, key)
	m.mu.Unlock()
	if g != nil {
		close(g.done)
	}
}

// Len reports committed key count (diagnostics/tests).
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.committed)
}

// memUoW is one in-memory transaction.
type memUoW struct {
	store    *MemStore
	key      string
	reserved bool
	staged   []func()
	rec      *Record
}

func (u *memUoW) Exec(_ context.Context, _ string, _ ...any) (int64, error) {
	// Staged writes are applied on Commit, discarded on Rollback — so a MemStore
	// business effect commits atomically with its idempotency key. Callers that
	// want to record an effect stage it via ExecFunc.
	return 1, nil
}

// ExecFunc stages an arbitrary in-process effect to run atomically on Commit.
// This is the MemStore equivalent of a business SQL write on the shared txn.
func (u *memUoW) ExecFunc(f func()) { u.staged = append(u.staged, f) }

func (u *memUoW) Insert(ctx context.Context, key, reqHash string) error {
	if err := u.store.reserve(ctx, key); err != nil {
		return err
	}
	u.key = key
	u.reserved = true
	u.rec = &Record{Key: key, ReqHash: reqHash, Status: StatusInFlight, CreatedAt: time.Now().UTC()}
	return nil
}

func (u *memUoW) SaveResponse(_ context.Context, key string, code int, body []byte) error {
	if u.rec == nil || u.rec.Key != key {
		return fmt.Errorf("idempotency: SaveResponse before Insert for %q", key)
	}
	u.rec.Status = StatusDone
	u.rec.Code = code
	// copy body so later mutation of the caller's buffer can't corrupt the record
	u.rec.Body = append([]byte(nil), body...)
	return nil
}

func (u *memUoW) Commit() error {
	if !u.reserved {
		return nil
	}
	for _, f := range u.staged {
		f()
	}
	u.store.resolve(u.key, u.rec)
	u.reserved = false
	return nil
}

func (u *memUoW) Rollback() error {
	if !u.reserved {
		return nil
	}
	u.store.resolve(u.key, nil) // release without committing; staged effects discarded
	u.reserved = false
	return nil
}
