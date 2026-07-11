package cache

import (
	"sync"
	"time"
)

// store.go — a TTL key/value store with fresh/stale/miss semantics. It is the
// in-process stand-in for Redis in this sandbox (no Redis daemon available): the
// SAME TTL contract a Redis `SET key val EX <ttl>` gives, implemented over a
// concurrent map read under the injected Clock. Disclosed in VERIFICATION.md
// §V-T6 — the singleflight + two-tier + stale-while-revalidate LOGIC that the
// correctness properties test is real and unchanged; only the backing store is
// in-process. (The idempotency lib's MemCache stands in for Redis the same way,
// but it is a response cache with no TTL — this store adds the TTL window +
// stale band the cache tiers need, so it lives here.)
//
// A value carries two horizons off its store time:
//   - fresh: now-stored < freshTTL          → serve directly, no revalidation
//   - stale: freshTTL <= now-stored < ttl    → serve stale + revalidate (SWR)
//   - miss:  now-stored >= ttl (or absent)   → cold, must fetch
//
// For the merchant two-tier cache the L1/L2 stores use freshTTL == ttl (no stale
// band): an entry is fresh until its TTL then a hard miss. For the geo-tile feed
// cache freshTTL < ttl carves out the stale-while-revalidate band.

// State classifies a lookup against the store's TTL windows.
type State int

const (
	// Miss — no live entry (absent or past its hard TTL).
	Miss State = iota
	// Fresh — within the fresh window; serve directly.
	Fresh
	// Stale — past fresh but within the hard TTL; serve stale + revalidate.
	Stale
)

func (s State) String() string {
	switch s {
	case Fresh:
		return "fresh"
	case Stale:
		return "stale"
	default:
		return "miss"
	}
}

type entry struct {
	val      []byte
	storedAt time.Time
}

// TTLStore is a concurrent TTL map with a fresh window and a hard TTL. Safe for
// concurrent use.
type TTLStore struct {
	clock    Clock
	freshTTL time.Duration
	ttl      time.Duration

	mu   sync.RWMutex
	data map[string]entry
}

// NewTTLStore builds a store. freshTTL is the no-revalidation window; ttl is the
// hard expiry (>= freshTTL). Pass freshTTL == ttl for a plain TTL cache with no
// stale band.
func NewTTLStore(clock Clock, freshTTL, ttl time.Duration) *TTLStore {
	if clock == nil {
		clock = SystemClock{}
	}
	if ttl < freshTTL {
		ttl = freshTTL
	}
	return &TTLStore{clock: clock, freshTTL: freshTTL, ttl: ttl, data: map[string]entry{}}
}

// Get classifies key against the TTL windows and returns the stored value when
// it is Fresh or Stale. On Miss the returned slice is nil.
func (s *TTLStore) Get(key string) ([]byte, State) {
	now := s.clock.Now()
	s.mu.RLock()
	e, ok := s.data[key]
	s.mu.RUnlock()
	if !ok {
		return nil, Miss
	}
	age := now.Sub(e.storedAt)
	switch {
	case age < s.freshTTL:
		return e.val, Fresh
	case age < s.ttl:
		return e.val, Stale
	default:
		return nil, Miss
	}
}

// Set stores/overwrites key with the current store time.
func (s *TTLStore) Set(key string, val []byte) {
	cp := append([]byte(nil), val...) // copy so a caller mutating its buffer can't corrupt the entry
	now := s.clock.Now()
	s.mu.Lock()
	s.data[key] = entry{val: cp, storedAt: now}
	s.mu.Unlock()
}

// Delete evicts a key (invalidation).
func (s *TTLStore) Delete(key string) {
	s.mu.Lock()
	delete(s.data, key)
	s.mu.Unlock()
}

// Len reports live-or-expired entry count (diagnostics/tests).
func (s *TTLStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
