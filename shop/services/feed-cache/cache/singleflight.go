package cache

import "sync"

// singleflight.go — the stampede-collapse primitive at the heart of this slice
// (D11 celebrity-merchant defence "(b) … two-tier cache (in-process
// singleflight-coalesced …)"; D17 "geo-tile feed cache with stampede protection
// (singleflight + stale-while-revalidate)"). This is OUR code (not a dependency)
// so the collapse property is proven directly against it.
//
// Group.Do coalesces concurrent calls for the SAME key into ONE execution of fn:
// the first caller (the leader) runs fn while every concurrent caller for that
// key BLOCKS and then shares the leader's single result. That is exactly the
// "cold-tile stampede (10k concurrent) ⇒ exactly 1 origin fetch" invariant — fn
// is the origin fetch, and it runs once no matter how many goroutines pile onto a
// cold key. Once fn returns, the key is released so a later (post-TTL) miss starts
// a fresh flight.

// call is one in-flight execution shared by all coalesced callers of a key.
type call struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

// Group coalesces duplicate concurrent calls keyed by string. The zero value is
// ready to use and safe for concurrent use by multiple goroutines.
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do executes fn for key, coalescing concurrent duplicates. It returns fn's
// result plus shared=true when the caller joined an in-flight leader's call
// (i.e. fn was NOT run for this caller). Exactly one fn runs per (key, flight),
// so callers can use shared to count how many requests a single origin fetch
// absorbed.
func (g *Group) Do(key string, fn func() ([]byte, error)) (val []byte, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		// A leader is already fetching this key — join it and share the result.
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	// We are the leader for this key.
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	// Only delete our own call (a later flight may have re-created the key after
	// we finished — defensive, though Do holds the key for the whole fn).
	if g.m[key] == c {
		delete(g.m, key)
	}
	g.mu.Unlock()
	return c.val, c.err, false
}

// InFlight reports the number of keys currently being fetched (diagnostics).
func (g *Group) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

// keyGuard is the NON-BLOCKING single-flight guard used for background
// revalidation (stale-while-revalidate): at most one revalidation may be
// in-flight per key. Unlike Group.Do (where late callers BLOCK and share the
// result), a stale request whose tile is already being revalidated must NOT
// block — it serves the stale value and skips. keyGuard.acquire gives exactly
// that: the first caller acquires and revalidates; every concurrent caller for
// the same key fails acquire and just serves stale. That guarantees a stale-tile
// stampede triggers EXACTLY ONE origin refetch. The zero value is ready to use.
type keyGuard struct {
	mu       sync.Mutex
	inflight map[string]bool
}

// acquire returns true iff no revalidation for key is already running (in which
// case the caller becomes the single revalidator and MUST call release).
func (g *keyGuard) acquire(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[key] {
		return false
	}
	if g.inflight == nil {
		g.inflight = make(map[string]bool)
	}
	g.inflight[key] = true
	return true
}

// release marks key's revalidation complete so a later (post-TTL) stale request
// can trigger a fresh one.
func (g *keyGuard) release(key string) {
	g.mu.Lock()
	delete(g.inflight, key)
	g.mu.Unlock()
}
