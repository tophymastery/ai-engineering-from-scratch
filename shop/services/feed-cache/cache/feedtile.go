package cache

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// feedtile.go — the GEO-TILE FEED CACHE with stale-while-revalidate (D17: "geo-
// tile feed cache with stampede protection (singleflight + stale-while-
// revalidate)"; C15: "stampede-protected geo-tile feed cache"). The customer
// browse feed (GET /v1/customer/home?lat=&lng=) is keyed by a GEO TILE (lat/lng
// quantised to a grid) so nearby users share one cached feed. Under SWR:
//
//	fresh  (age < freshTTL)          → serve cached, no origin work
//	stale  (freshTTL <= age < ttl)   → serve the STALE feed IMMEDIATELY (fast,
//	                                   never blocks the user) AND kick ONE
//	                                   background revalidation (singleflight-
//	                                   coalesced per tile — a stale-tile stampede
//	                                   triggers exactly one origin refetch)
//	miss   (age >= ttl / absent)     → block on the ONE origin fetch (also
//	                                   singleflight-coalesced: a cold-tile
//	                                   stampede ⇒ exactly 1 origin fetch)
//
// The hit-rate property (feed cache hit ≥ 85% at peak profile) counts fresh AND
// stale-served responses as hits (SWR serves both from cache); only a cold miss
// blocks on the origin. Measured genuinely over a tile-skewed (Zipfian) profile
// in feedtile_test.go.

// tileGrid is the geo-tile size (~1.1 km at the equator). Nearby users quantise
// onto one tile so they share a single cached feed.
const tileGrid = 0.01

// TileFor quantises a lat/lng to a grid cell id. Deterministic and allocation-
// light (string key).
func TileFor(lat, lng float64) string {
	qy := int(math.Floor(lat / tileGrid))
	qx := int(math.Floor(lng / tileGrid))
	return fmt.Sprintf("t:%d:%d", qy, qx)
}

// TileCenter returns the center lat/lng of a tile id produced by TileFor. The
// feed origin is fetched at the tile CENTER (all users in a tile get the tile's
// feed — the point of a geo-tile cache), so the cache key (tile) round-trips to
// the one origin request that fills it.
func TileCenter(tile string) (lat, lng float64, ok bool) {
	var qy, qx int
	if _, err := fmt.Sscanf(tile, "t:%d:%d", &qy, &qx); err != nil {
		return 0, 0, false
	}
	return (float64(qy) + 0.5) * tileGrid, (float64(qx) + 0.5) * tileGrid, true
}

// FeedCache is the geo-tile SWR cache. Construct with NewFeedCache.
type FeedCache struct {
	store      *TTLStore
	sf         Group    // coalesces the blocking cold-miss fetches
	revalGuard keyGuard // at most one background revalidation per tile (SWR)
	origin     Origin
	clock      Clock

	// background revalidation bookkeeping so tests can await async refreshes
	// deterministically (WaitRevalidations) instead of sleeping.
	revalWG sync.WaitGroup

	// metrics
	requests      atomic.Int64
	freshHits     atomic.Int64
	staleHits     atomic.Int64
	misses        atomic.Int64
	revalidations atomic.Int64
	originFetches atomic.Int64
	coalesced     atomic.Int64
}

// NewFeedCache builds the feed cache. freshTTL is the no-revalidation window;
// staleTTL is the additional band during which a stale feed is served while a
// background refresh runs (hard expiry = freshTTL + staleTTL).
func NewFeedCache(clock Clock, origin Origin, freshTTL, staleTTL time.Duration) *FeedCache {
	if clock == nil {
		clock = SystemClock{}
	}
	if freshTTL <= 0 {
		freshTTL = 30 * time.Second
	}
	if staleTTL <= 0 {
		staleTTL = 5 * time.Minute
	}
	return &FeedCache{
		store:  NewTTLStore(clock, freshTTL, freshTTL+staleTTL),
		origin: origin,
		clock:  clock,
	}
}

// FeedResult carries a served feed and how it was served.
type FeedResult struct {
	Value      []byte
	State      string // "fresh" | "stale" | "miss"
	Revalidate bool   // a background revalidation was kicked (stale path)
}

// Get serves the feed for a tile under SWR. Fresh and stale are served from the
// cache (a hit); stale additionally kicks one background revalidation; a cold
// miss blocks on the single (coalesced) origin fetch.
func (f *FeedCache) Get(ctx context.Context, tile string, header http.Header) (FeedResult, error) {
	f.requests.Add(1)
	v, st := f.store.Get(tile)
	switch st {
	case Fresh:
		f.freshHits.Add(1)
		return FeedResult{Value: v, State: "fresh"}, nil
	case Stale:
		f.staleHits.Add(1)
		f.kickRevalidate(tile, header)
		return FeedResult{Value: v, State: "stale", Revalidate: true}, nil
	default: // Miss
		f.misses.Add(1)
		val, err, shared := f.sf.Do(tile, func() ([]byte, error) {
			// Re-check under the flight in case a sibling just filled it.
			if v, st := f.store.Get(tile); st == Fresh {
				return v, nil
			}
			b, e := f.origin.Fetch(ctx, tile, header)
			if e != nil {
				return nil, e
			}
			f.originFetches.Add(1)
			f.store.Set(tile, b)
			return b, nil
		})
		if err != nil {
			return FeedResult{}, err
		}
		if shared {
			f.coalesced.Add(1)
		}
		return FeedResult{Value: val, State: "miss"}, nil
	}
}

// Bypass fetches straight from the origin with no caching (feed_cache flag OFF or
// an X-Flag-Override request — deterministic testing must not read/write the
// shared cache). Forwards header (e.g. the override) to the origin.
func (f *FeedCache) Bypass(ctx context.Context, tile string, header http.Header) (FeedResult, error) {
	f.requests.Add(1)
	b, err := f.origin.Fetch(ctx, tile, header)
	if err != nil {
		return FeedResult{}, err
	}
	f.originFetches.Add(1)
	return FeedResult{Value: b, State: "bypass"}, nil
}

// kickRevalidate starts AT MOST ONE background refresh per tile. The guard is
// acquired SYNCHRONOUSLY here (in the request goroutine), so a stale-tile
// stampede — every concurrent stale request calling this — results in exactly one
// acquirer; the rest fail acquire and simply serve stale. The single acquirer's
// origin fetch runs off the request path so the user is never blocked.
func (f *FeedCache) kickRevalidate(tile string, header http.Header) {
	if !f.revalGuard.acquire(tile) {
		return // a revalidation for this tile is already in flight — serve stale, skip
	}
	// Copy only the forwarded headers we need; the request may be gone by the
	// time the goroutine runs.
	h := http.Header{}
	if header != nil {
		if v := header.Get(flagOverrideHeader); v != "" {
			h.Set(flagOverrideHeader, v)
		}
	}
	f.revalWG.Add(1)
	go func() {
		defer f.revalWG.Done()
		defer f.revalGuard.release(tile)
		b, e := f.origin.Fetch(context.Background(), tile, h)
		if e != nil {
			return // keep serving stale; the next stale request retries
		}
		f.originFetches.Add(1)
		f.revalidations.Add(1)
		f.store.Set(tile, b)
	}()
}

// WaitRevalidations blocks until all in-flight background revalidations finish.
// Test-only determinism hook (advance clock → Get → WaitRevalidations → assert),
// so the SWR tests never sleep on wall time.
func (f *FeedCache) WaitRevalidations() { f.revalWG.Wait() }

// flagOverrideHeader mirrors libs/flags.OverrideHeader; duplicated as a literal
// so this package needs no dependency on flags (the header name is a stable
// wire contract, gateway-stripped in prod).
const flagOverrideHeader = "X-Flag-Override"

// FeedStats is the metrics snapshot for /v1/cache/stats.
type FeedStats struct {
	Requests      int64   `json:"requests"`
	FreshHits     int64   `json:"fresh_hits"`
	StaleHits     int64   `json:"stale_hits"`
	Misses        int64   `json:"misses"`
	Revalidations int64   `json:"revalidations"`
	OriginFetches int64   `json:"origin_fetches"`
	Coalesced     int64   `json:"coalesced"`
	HitRate       float64 `json:"hit_rate"`
	Tiles         int     `json:"tiles"`
}

// Stats returns a snapshot. HitRate = (fresh + stale) / requests — the fraction
// served from cache under SWR (the ≥85% property).
func (f *FeedCache) Stats() FeedStats {
	req := f.requests.Load()
	fresh := f.freshHits.Load()
	stale := f.staleHits.Load()
	hr := 1.0
	if req > 0 {
		hr = float64(fresh+stale) / float64(req)
	}
	return FeedStats{
		Requests:      req,
		FreshHits:     fresh,
		StaleHits:     stale,
		Misses:        f.misses.Load(),
		Revalidations: f.revalidations.Load(),
		OriginFetches: f.originFetches.Load(),
		Coalesced:     f.coalesced.Load(),
		HitRate:       hr,
		Tiles:         f.store.Len(),
	}
}

// OriginFetches is the raw origin-call count (cold-tile stampede asserts ==1).
func (f *FeedCache) OriginFetches() int64 { return f.originFetches.Load() }

// HitRate is the current cache hit rate (fresh+stale)/requests.
func (f *FeedCache) HitRate() float64 { return f.Stats().HitRate }
