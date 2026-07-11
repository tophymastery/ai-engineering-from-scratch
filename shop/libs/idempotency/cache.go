package idempotency

import (
	"context"
	"sync"
	"sync/atomic"
)

// Cache is the D9 advisory layer: a read-through response cache plus an
// IN_FLIGHT marker for fast double-tap rejection. It is NEVER the source of
// truth — a missing/stale/emptied cache only costs a DB round-trip. Prod backs
// this with Redis; MemCache backs it here, interface-compatible.
type Cache interface {
	// Get returns a cached record. ok=false on miss.
	Get(ctx context.Context, key string) (Record, bool)
	// Set stores/overwrites the response copy for key.
	Set(ctx context.Context, key string, rec Record)
	// SetInFlight records an advisory IN_FLIGHT marker (best-effort).
	SetInFlight(ctx context.Context, key, reqHash string)
	// Delete evicts a key.
	Delete(ctx context.Context, key string)
}

// MemCache is an in-memory Cache standing in for Redis. Safe for concurrent use.
type MemCache struct {
	mu   sync.RWMutex
	data map[string]Record
}

// NewMemCache builds an empty in-memory cache.
func NewMemCache() *MemCache { return &MemCache{data: map[string]Record{}} }

func (c *MemCache) Get(_ context.Context, key string) (Record, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.data[key]
	return r, ok
}

func (c *MemCache) Set(_ context.Context, key string, rec Record) {
	c.mu.Lock()
	c.data[key] = rec
	c.mu.Unlock()
}

func (c *MemCache) SetInFlight(_ context.Context, key, reqHash string) {
	c.mu.Lock()
	if _, exists := c.data[key]; !exists {
		c.data[key] = Record{Key: key, ReqHash: reqHash, Status: StatusInFlight}
	}
	c.mu.Unlock()
}

func (c *MemCache) Delete(_ context.Context, key string) {
	c.mu.Lock()
	delete(c.data, key)
	c.mu.Unlock()
}

// Len reports the number of cached entries (test/diagnostics).
func (c *MemCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// SwappableCache wraps an inner Cache behind an atomic pointer so a test (or a
// chaos drill) can DROP the cache mid-flight — simulating a Redis failover /
// FLUSHALL. After Drop, every op is a no-op/miss, forcing the durable DB path.
// This is how the "cache killed mid-storm ⇒ still exactly 1 effect" criterion
// is exercised.
type SwappableCache struct {
	inner atomic.Pointer[cacheHolder]
}

type cacheHolder struct{ c Cache }

// NewSwappableCache wraps inner.
func NewSwappableCache(inner Cache) *SwappableCache {
	s := &SwappableCache{}
	s.inner.Store(&cacheHolder{c: inner})
	return s
}

// Drop detaches the inner cache: all subsequent ops behave as a Redis outage.
func (s *SwappableCache) Drop() { s.inner.Store(&cacheHolder{c: nil}) }

// Dropped reports whether the cache has been dropped.
func (s *SwappableCache) Dropped() bool { return s.inner.Load().c == nil }

func (s *SwappableCache) Get(ctx context.Context, key string) (Record, bool) {
	if c := s.inner.Load().c; c != nil {
		return c.Get(ctx, key)
	}
	return Record{}, false
}

func (s *SwappableCache) Set(ctx context.Context, key string, rec Record) {
	if c := s.inner.Load().c; c != nil {
		c.Set(ctx, key, rec)
	}
}

func (s *SwappableCache) SetInFlight(ctx context.Context, key, reqHash string) {
	if c := s.inner.Load().c; c != nil {
		c.SetInFlight(ctx, key, reqHash)
	}
}

func (s *SwappableCache) Delete(ctx context.Context, key string) {
	if c := s.inner.Load().c; c != nil {
		c.Delete(ctx, key)
	}
}
