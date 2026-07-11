package cache

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

// twotier.go — the MERCHANT-PAGE two-tier cache (D11 celebrity-merchant defence
// (b)): "merchant/store pages served from two-tier cache (in-process
// singleflight-coalesced 1 s TTL over Redis 10 s TTL) ⇒ a 1M-RPS merchant page
// costs catalog ≤1 QPS".
//
// Two tiers + a singleflight collapse the load:
//
//	L1  in-process, 1 s TTL   — the hot per-node tier; a fresh L1 hit never even
//	                            touches L2, so steady-state cost is ~one map read.
//	SF  singleflight           — on an L1 miss, concurrent requests for the SAME
//	                            merchant collapse to ONE filler; the rest share it.
//	L2  Redis-like, 10 s TTL   — the shared tier (in-process stand-in here); the
//	                            filler reads L2 first, so the origin is only hit
//	                            when BOTH tiers miss (i.e. ~once per 10 s per key).
//	origin  catalog            — the authoritative source, protected to ≤1 QPS.
//
// Correctness properties proven against this type (VERIFICATION.md §V-T6):
//   - cold-key stampede (10k concurrent) ⇒ EXACTLY 1 origin fetch (OriginFetches
//     counts real calls; the leader runs once, 9 999 share it) — under -race.
//   - sustained load on one warm key ⇒ ≤1 origin QPS (L2's 10 s TTL bounds the
//     origin refresh rate; L1's 1 s TTL bounds L2 reads).

// Result is the outcome of a Get, carrying the value and where it came from.
type Result struct {
	Value  []byte
	Tier   string // "l1" | "l2" | "origin" | "bypass"
	Shared bool   // joined an in-flight singleflight leader (coalesced)
}

// TwoTier is the merchant-page cache. Construct with NewTwoTier.
type TwoTier struct {
	l1     *TTLStore
	l2     *TTLStore
	sf     Group
	origin Origin

	// metrics (atomic; read by the /v1/cache/stats endpoint and the tests).
	l1Hits        atomic.Int64
	l2Hits        atomic.Int64
	originFetches atomic.Int64
	coalesced     atomic.Int64
	requests      atomic.Int64
}

// NewTwoTier builds the cache with the D11 default TTLs (L1 1 s over L2 10 s).
// Pass explicit TTLs for tests that need a tighter window.
func NewTwoTier(clock Clock, origin Origin, l1TTL, l2TTL time.Duration) *TwoTier {
	if l1TTL <= 0 {
		l1TTL = 1 * time.Second
	}
	if l2TTL <= 0 {
		l2TTL = 10 * time.Second
	}
	return &TwoTier{
		l1:     NewTTLStore(clock, l1TTL, l1TTL), // no stale band: fresh until TTL
		l2:     NewTTLStore(clock, l2TTL, l2TTL),
		origin: origin,
	}
}

// Get returns the merchant page for key, filling the tiers on a miss. On an L1
// miss it enters the singleflight so a cold-key stampede collapses to one filler;
// the filler consults L2 before hitting the origin, so the origin is touched only
// when both tiers are cold.
func (t *TwoTier) Get(ctx context.Context, key string) (Result, error) {
	t.requests.Add(1)

	// Fast path: a fresh L1 hit never touches L2 or the singleflight.
	if v, st := t.l1.Get(key); st == Fresh {
		t.l1Hits.Add(1)
		return Result{Value: v, Tier: "l1"}, nil
	}

	// L1 miss: collapse concurrent fillers for this key into one.
	var tier string
	val, err, shared := t.sf.Do(key, func() ([]byte, error) {
		// Re-read L2 inside the flight: another node/window may have filled it.
		if v, st := t.l2.Get(key); st == Fresh {
			t.l2Hits.Add(1)
			t.l1.Set(key, v)
			tier = "l2"
			return v, nil
		}
		// Both tiers cold: the ONE origin fetch for this flight.
		v, e := t.origin.Fetch(ctx, key, nil)
		if e != nil {
			return nil, e
		}
		t.originFetches.Add(1)
		t.l2.Set(key, v)
		t.l1.Set(key, v)
		tier = "origin"
		return v, nil
	})
	if err != nil {
		return Result{}, err
	}
	if shared {
		t.coalesced.Add(1)
		// A coalesced caller shares the leader's fill; its effective tier is
		// whatever the leader produced (l2 or origin) but it did NO origin work.
		if tier == "" {
			tier = "l1" // leader filled L1; we observed the shared value
		}
	}
	return Result{Value: val, Tier: tier, Shared: shared}, nil
}

// Bypass fetches straight from the origin with no caching (feed_cache flag OFF,
// or an X-Flag-Override request that must not read/write the shared cache). It
// still counts as an origin fetch.
func (t *TwoTier) Bypass(ctx context.Context, key string, header http.Header) (Result, error) {
	t.requests.Add(1)
	v, err := t.origin.Fetch(ctx, key, header)
	if err != nil {
		return Result{}, err
	}
	t.originFetches.Add(1)
	return Result{Value: v, Tier: "bypass"}, nil
}

// Invalidate evicts a merchant from both tiers (e.g. on a menu.updated event).
func (t *TwoTier) Invalidate(key string) {
	t.l1.Delete(key)
	t.l2.Delete(key)
}

// TwoTierStats is the metrics snapshot for /v1/cache/stats.
type TwoTierStats struct {
	Requests      int64   `json:"requests"`
	L1Hits        int64   `json:"l1_hits"`
	L2Hits        int64   `json:"l2_hits"`
	OriginFetches int64   `json:"origin_fetches"`
	Coalesced     int64   `json:"coalesced"`
	HitRate       float64 `json:"hit_rate"`
}

// Stats returns a metrics snapshot. HitRate is the fraction of requests served
// without an origin fetch (L1 + L2 + coalesced).
func (t *TwoTier) Stats() TwoTierStats {
	req := t.requests.Load()
	of := t.originFetches.Load()
	hr := 1.0
	if req > 0 {
		hr = float64(req-of) / float64(req)
	}
	return TwoTierStats{
		Requests:      req,
		L1Hits:        t.l1Hits.Load(),
		L2Hits:        t.l2Hits.Load(),
		OriginFetches: of,
		Coalesced:     t.coalesced.Load(),
		HitRate:       hr,
	}
}

// OriginFetches is the raw origin-call count (the stampede invariant asserts ==1).
func (t *TwoTier) OriginFetches() int64 { return t.originFetches.Load() }
