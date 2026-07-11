package cache

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestTwoTier_ColdStampedeExactlyOneOriginFetch is THE headline correctness
// proof of this slice (V-T6 test criterion: "cold-tile stampede (10k concurrent)
// ⇒ exactly 1 origin fetch"). 10,000 goroutines hit a COLD merchant key
// concurrently; the origin's atomic counter must read EXACTLY 1. Real
// concurrency, real count, run under -race in the standard suite.
func TestTwoTier_ColdStampedeExactlyOneOriginFetch(t *testing.T) {
	origin := &countingOrigin{gate: make(chan struct{})}
	c := NewTwoTier(SystemClock{}, origin, time.Second, 10*time.Second)

	const n = 10000
	var started, done sync.WaitGroup
	started.Add(n)
	done.Add(n)
	vals := make([][]byte, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			started.Done()
			res, err := c.Get(context.Background(), "mer_hot")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			vals[i] = res.Value
		}(i)
	}
	started.Wait()     // all 10k goroutines scheduled
	close(origin.gate) // release the single in-flight leader fetch
	done.Wait()

	if got := origin.count(); got != 1 {
		t.Fatalf("origin fetched %d times under a 10k stampede, want EXACTLY 1", got)
	}
	st := c.Stats()
	if st.OriginFetches != 1 {
		t.Fatalf("stats OriginFetches=%d, want 1", st.OriginFetches)
	}
	if st.Requests != n {
		t.Fatalf("stats Requests=%d, want %d", st.Requests, n)
	}
	// Every caller saw the same single fetched value.
	first := string(vals[0])
	for i, v := range vals {
		if string(v) != first {
			t.Fatalf("caller %d saw %q, want the single origin value %q", i, v, first)
		}
	}
	// The 9,999 non-leaders were coalesced onto the leader's flight.
	if st.Coalesced < n-1 {
		t.Fatalf("coalesced=%d, want >= %d", st.Coalesced, n-1)
	}
	t.Logf("10k cold stampede: origin_fetches=%d coalesced=%d hit_rate=%.4f",
		st.OriginFetches, st.Coalesced, st.HitRate)
}

// TestTwoTier_L1FastPathAndL2Refill confirms the tier mechanics on an advancing
// ManualClock: a fresh L1 hit never touches the origin; after L1 expires the
// refill comes from L2 (still no origin fetch); after L2 expires the origin is
// hit again — i.e. the origin refresh rate is bounded by the L2 (10s) TTL.
func TestTwoTier_L1FastPathAndL2Refill(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	origin := &countingOrigin{}
	c := NewTwoTier(clk, origin, time.Second, 10*time.Second)
	ctx := context.Background()

	r, _ := c.Get(ctx, "m") // cold → origin
	if r.Tier != "origin" || origin.count() != 1 {
		t.Fatalf("first get tier=%s fetches=%d, want origin/1", r.Tier, origin.count())
	}
	r, _ = c.Get(ctx, "m") // fresh L1
	if r.Tier != "l1" || origin.count() != 1 {
		t.Fatalf("second get tier=%s fetches=%d, want l1/1", r.Tier, origin.count())
	}
	clk.Advance(1500 * time.Millisecond) // L1 expired (1s), L2 still fresh (10s)
	r, _ = c.Get(ctx, "m")
	if r.Tier != "l2" || origin.count() != 1 {
		t.Fatalf("post-L1 get tier=%s fetches=%d, want l2/1 (no origin)", r.Tier, origin.count())
	}
	clk.Advance(10 * time.Second) // now past L2 TTL
	r, _ = c.Get(ctx, "m")
	if r.Tier != "origin" || origin.count() != 2 {
		t.Fatalf("post-L2 get tier=%s fetches=%d, want origin/2", r.Tier, origin.count())
	}
}

// TestTwoTier_Bypass confirms the flag-off / override path skips both tiers and
// forwards headers to the origin.
func TestTwoTier_Bypass(t *testing.T) {
	origin := &countingOrigin{}
	c := NewTwoTier(SystemClock{}, origin, time.Second, 10*time.Second)
	h := http.Header{}
	h.Set("X-Flag-Override", "feed_cache=false")
	for i := 0; i < 5; i++ {
		r, err := c.Bypass(context.Background(), "m", h)
		if err != nil || r.Tier != "bypass" {
			t.Fatalf("bypass %d: tier=%s err=%v", i, r.Tier, err)
		}
	}
	if origin.count() != 5 {
		t.Fatalf("bypass hit origin %d times, want 5 (no caching)", origin.count())
	}
	got, _ := origin.lastHdr.Load().(http.Header)
	if got.Get("X-Flag-Override") != "feed_cache=false" {
		t.Fatalf("override header not forwarded to origin: %v", got)
	}
}

// TestTwoTier_Invalidate confirms an explicit invalidation forces the next Get to
// re-fetch (both tiers dropped).
func TestTwoTier_Invalidate(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	origin := &countingOrigin{}
	c := NewTwoTier(clk, origin, time.Second, 10*time.Second)
	ctx := context.Background()
	_, _ = c.Get(ctx, "m")
	c.Invalidate("m")
	r, _ := c.Get(ctx, "m")
	if r.Tier != "origin" || origin.count() != 2 {
		t.Fatalf("after invalidate tier=%s fetches=%d, want origin/2", r.Tier, origin.count())
	}
}
