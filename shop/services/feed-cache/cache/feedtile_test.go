package cache

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// TestFeedCache_StaleWhileRevalidate proves the SWR mechanic on a ManualClock:
// a stale request serves the OLD feed immediately AND kicks exactly one
// background revalidation that refreshes the tile.
func TestFeedCache_StaleWhileRevalidate(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	origin := &countingOrigin{}
	f := NewFeedCache(clk, origin, 2*time.Second, 8*time.Second) // fresh 2s, stale band 8s
	ctx := context.Background()

	// 1. cold miss → origin fetch #1 (value v=1), served as "miss".
	r, _ := f.Get(ctx, "tile", nil)
	if r.State != "miss" || origin.count() != 1 {
		t.Fatalf("first get state=%s fetches=%d, want miss/1", r.State, origin.count())
	}
	v1 := string(r.Value)

	// 2. fresh hit → no origin work.
	r, _ = f.Get(ctx, "tile", nil)
	if r.State != "fresh" || origin.count() != 1 {
		t.Fatalf("fresh get state=%s fetches=%d, want fresh/1", r.State, origin.count())
	}

	// 3. advance into the stale band → stale serve (OLD value) + one background
	//    revalidation (origin fetch #2, value v=2).
	clk.Advance(3 * time.Second) // age 3s: past fresh(2s), within hard(10s)
	r, _ = f.Get(ctx, "tile", nil)
	if r.State != "stale" || !r.Revalidate {
		t.Fatalf("stale get state=%s revalidate=%v, want stale/true", r.State, r.Revalidate)
	}
	if string(r.Value) != v1 {
		t.Fatalf("stale serve returned %q, want the OLD value %q (served immediately)", r.Value, v1)
	}
	f.WaitRevalidations() // deterministic: wait for the background refresh
	if origin.count() != 2 || f.Stats().Revalidations != 1 {
		t.Fatalf("after reval fetches=%d revalidations=%d, want 2/1", origin.count(), f.Stats().Revalidations)
	}

	// 4. next get is fresh again with the REVALIDATED value (v=2).
	r, _ = f.Get(ctx, "tile", nil)
	if r.State != "fresh" || string(r.Value) == v1 {
		t.Fatalf("post-reval get state=%s value=%q, want fresh with refreshed value", r.State, r.Value)
	}
}

// TestFeedCache_StaleStampedeOneRevalidation proves a stale-tile stampede (many
// concurrent stale requests) triggers EXACTLY ONE background revalidation — the
// revalidation singleflight collapses the refetch. Under -race.
func TestFeedCache_StaleStampedeOneRevalidation(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	origin := &countingOrigin{}
	f := NewFeedCache(clk, origin, 2*time.Second, 8*time.Second)
	ctx := context.Background()

	_, _ = f.Get(ctx, "tile", nil) // warm it (fetch #1)
	before := origin.count()

	// Gate the revalidation origin fetch so every stale request is issued before
	// the single leader refetch completes.
	origin.gate = make(chan struct{})
	clk.Advance(3 * time.Second) // into the stale band

	const n = 2000
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r, err := f.Get(ctx, "tile", nil)
			if err != nil || r.State != "stale" {
				t.Errorf("stale get state=%s err=%v", r.State, err)
			}
		}()
	}
	wg.Wait()          // all 2000 stale serves returned (they never block)
	close(origin.gate) // release the single background revalidation leader
	f.WaitRevalidations()

	if got := origin.count() - before; got != 1 {
		t.Fatalf("stale stampede triggered %d origin refetches, want EXACTLY 1", got)
	}
	if f.Stats().Revalidations != 1 {
		t.Fatalf("revalidations=%d, want 1", f.Stats().Revalidations)
	}
}

// TestFeedCache_ColdStampedeExactlyOneOriginFetch proves a COLD-tile stampede
// (10k concurrent) collapses to exactly 1 blocking origin fetch — the feed-cache
// analogue of the merchant two-tier invariant. Under -race.
func TestFeedCache_ColdStampedeExactlyOneOriginFetch(t *testing.T) {
	origin := &countingOrigin{gate: make(chan struct{})}
	f := NewFeedCache(SystemClock{}, origin, 30*time.Second, time.Minute)

	const n = 10000
	var started, done sync.WaitGroup
	started.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			started.Done()
			if _, err := f.Get(context.Background(), "cold_tile", nil); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	started.Wait()
	close(origin.gate)
	done.Wait()
	if got := origin.count(); got != 1 {
		t.Fatalf("cold feed stampede fetched origin %d times, want EXACTLY 1", got)
	}
}

// TestFeedCache_HitRateAtPeakProfile is the ≥85% hit-rate proof (V-T6 test
// criterion). It replays a realistic PEAK request profile — Zipfian tile skew
// over an advancing clock so time-based staleness genuinely evicts cold tiles —
// and measures the real hit rate (fresh + stale served, i.e. everything not a
// cold origin block). Deterministic (seeded RNG + ManualClock).
func TestFeedCache_HitRateAtPeakProfile(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	origin := &countingOrigin{}
	// Peak-cell TTLs: 30s fresh + 5min stale band (production defaults). The
	// stream below spans ~50s of simulated time.
	f := NewFeedCache(clk, origin, 30*time.Second, 5*time.Minute)
	ctx := context.Background()

	const (
		requests = 50000
		tiles    = 1000
		dt       = time.Millisecond // 1ms between requests → 50s of traffic
	)
	rng := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(rng, 1.3, 1, tiles-1) // s=1.3: strong tile skew (a few hot tiles)

	for i := 0; i < requests; i++ {
		tile := TileFor(13.0+float64(zipf.Uint64())*0.01, 100.0)
		if _, err := f.Get(ctx, tile, nil); err != nil {
			t.Fatalf("get: %v", err)
		}
		f.WaitRevalidations() // keep the measurement deterministic (no async races)
		clk.Advance(dt)
	}

	st := f.Stats()
	if st.HitRate < 0.85 {
		t.Fatalf("feed cache hit rate %.4f < 0.85 at peak profile (fresh=%d stale=%d miss=%d)",
			st.HitRate, st.FreshHits, st.StaleHits, st.Misses)
	}
	t.Logf("peak profile: requests=%d hit_rate=%.4f (fresh=%d stale=%d miss=%d) origin_fetches=%d distinct_tiles~%d",
		st.Requests, st.HitRate, st.FreshHits, st.StaleHits, st.Misses, st.OriginFetches, st.Tiles)
}
