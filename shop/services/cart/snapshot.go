package main

import (
	"sync"
	"time"
)

// snapshot.go — the REDIS-SNAPSHOT tier (01 §1 "cart: Redis snapshot + PG"). It
// is the in-process stand-in for Redis in this sandbox (no Redis daemon here):
// the SAME TTL contract a Redis `SET cart:<id> <json> EX <ttl>` gives — an
// assembled cart view is cached for a freshness window, then a hard miss forces
// a rehydrate from PostgreSQL (the durable system of record). Disclosed in
// VERIFICATION.md §V-T7; the snapshot/rehydrate LOGIC is real and fully tested
// (the idempotency lib's MemCache stands in for Redis the same way — this store
// adds the TTL window the snapshot tier needs, so it lives here). It reuses the
// V-T6 feed-cache TTLStore SHAPE (a concurrent TTL map under an injected Clock)
// specialised to the cart snapshot: a single freshness horizon plus explicit
// invalidation on a menu-change revalidation.
//
// Freshness window = snapshotTTL (default 5 s, the "menu-change reflected < 5 s"
// bound, 01 §1 test criteria): a cached view is served for up to snapshotTTL,
// after which the next read rehydrates from PG (which the menu.updated consumer
// has already repriced) — so a menu change is reflected within the window even
// if the eager per-cart invalidation is missed. The consumer ALSO invalidates
// the affected snapshots eagerly, making reflection immediate on the next read.

type snapEntry struct {
	val      []byte
	storedAt time.Time
}

// snapshotStore is a concurrent TTL cache of assembled cart views keyed by
// cart_id. Safe for concurrent use (request goroutines + the menu.updated
// consumer goroutine touch it under -race).
type snapshotStore struct {
	clock Clock
	ttl   time.Duration

	mu   sync.RWMutex
	data map[string]snapEntry

	// audit counters (dashboards + snapshot/rehydrate proof).
	hits     int64
	misses   int64
	rehydr   int64 // rehydrates from PG on a snapshot miss
	invalid  int64 // eager invalidations from a menu-change revalidation
	setCalls int64
}

// newSnapshotStore builds the snapshot tier with a freshness TTL.
func newSnapshotStore(clock Clock, ttl time.Duration) *snapshotStore {
	if clock == nil {
		clock = SystemClock{}
	}
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &snapshotStore{clock: clock, ttl: ttl, data: map[string]snapEntry{}}
}

// get returns the cached view bytes when the entry is present and still within
// the freshness window. A miss (absent or past TTL) returns ok=false, which the
// read path handles by rehydrating from PG.
func (s *snapshotStore) get(cartID string) ([]byte, bool) {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[cartID]
	if !ok || now.Sub(e.storedAt) >= s.ttl {
		s.misses++
		return nil, false
	}
	s.hits++
	return e.val, true
}

// set stores/overwrites the cached view with the current store time.
func (s *snapshotStore) set(cartID string, val []byte) {
	cp := append([]byte(nil), val...) // copy so a caller mutating its buffer can't corrupt the entry
	now := s.clock.Now()
	s.mu.Lock()
	s.data[cartID] = snapEntry{val: cp, storedAt: now}
	s.setCalls++
	s.mu.Unlock()
}

// invalidate evicts a cart's snapshot (menu-change revalidation / cart eviction),
// forcing the next read to rehydrate from PG.
func (s *snapshotStore) invalidate(cartID string) {
	s.mu.Lock()
	if _, ok := s.data[cartID]; ok {
		delete(s.data, cartID)
		s.invalid++
	}
	s.mu.Unlock()
}

// markRehydrate records a PG rehydrate (snapshot miss → load from the durable
// store → repopulate the snapshot). Used by the read path + the snapshot/rehydrate
// proof.
func (s *snapshotStore) markRehydrate() {
	s.mu.Lock()
	s.rehydr++
	s.mu.Unlock()
}

// stats returns a copy of the audit counters (dashboards / tests).
func (s *snapshotStore) stats() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]int64{
		"hits":         s.hits,
		"misses":       s.misses,
		"rehydrates":   s.rehydr,
		"invalidations": s.invalid,
		"sets":         s.setCalls,
		"entries":      int64(len(s.data)),
	}
}
