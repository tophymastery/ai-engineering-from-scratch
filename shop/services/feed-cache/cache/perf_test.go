//go:build !race

// perf_test.go — the THROUGHPUT-adapted proofs (V-T6 test criterion "1M RPS on
// one merchant page ⇒ origin ≤ 1 QPS"). A literal 1M requests/second is not
// reachable in-sandbox, so these measure the two things that ARE the real
// invariant and disclose the throughput adaptation (VERIFICATION.md §V-T6):
//
//   - the COLLAPSE RATIO: 1,000,000 requests to one warm merchant page cost the
//     origin exactly ONE fetch (a stronger statement than "≤1 QPS" — a million
//     requests, one fetch);
//   - the SUSTAINED RATE: continuous concurrent load for >2s (crossing the L1 1s
//     TTL) keeps the origin at ≤1 QPS because L2's 10s TTL bounds the refresh.
//
// Tagged !race because race instrumentation invalidates throughput/timing; the
// EXACTLY-1 correctness fixtures run under -race in the standard suite.
package cache

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPerf_MillionRequestsOneMerchantOneOriginFetch fires 1,000,000 concurrent
// Get calls at one warm merchant key within the L2 window and asserts the origin
// was fetched exactly once — the "1M requests ⇒ origin ≤ 1" collapse, measured.
func TestPerf_MillionRequestsOneMerchantOneOriginFetch(t *testing.T) {
	origin := &countingOrigin{}
	// Wide L2 window so the whole 1M-request run stays warm (isolates the
	// collapse ratio from TTL refresh, which the sustained test covers).
	c := NewTwoTier(SystemClock{}, origin, time.Second, time.Minute)
	ctx := context.Background()

	c.Get(ctx, "mer_hot") // prime the cache (the one legitimate origin fetch)

	const total = 1_000_000
	workers := runtime.GOMAXPROCS(0) * 4
	var served atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	per := total / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := c.Get(ctx, "mer_hot"); err == nil {
					served.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if of := origin.count(); of != 1 {
		t.Fatalf("1M requests cost the origin %d fetches, want EXACTLY 1", of)
	}
	rps := float64(served.Load()) / elapsed.Seconds()
	t.Logf("1M-request collapse: served=%d in %s (%.0f req/s in-proc) ⇒ origin_fetches=1 (origin QPS=%.4f)",
		served.Load(), elapsed.Round(time.Millisecond), rps, 1.0/elapsed.Seconds())
}

// TestPerf_SustainedLoadOriginBelowOneQPS runs continuous concurrent load on one
// key for >2s (crossing the L1 1s TTL several times) and asserts the origin stays
// at ≤1 QPS — L2's 10s TTL bounds the origin refresh; L1 misses are absorbed by
// L2, never the origin.
func TestPerf_SustainedLoadOriginBelowOneQPS(t *testing.T) {
	origin := &countingOrigin{}
	c := NewTwoTier(SystemClock{}, origin, time.Second, 10*time.Second) // D11 defaults
	ctx := context.Background()

	const dur = 2500 * time.Millisecond
	deadline := time.Now().Add(dur)
	workers := runtime.GOMAXPROCS(0) * 4
	var served atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := c.Get(ctx, "mer_hot"); err == nil {
					served.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	of := origin.count()
	originQPS := float64(of) / elapsed.Seconds()
	if originQPS > 1.0 {
		t.Fatalf("origin QPS = %.3f (%d fetches / %s) > 1.0", originQPS, of, elapsed)
	}
	st := c.Stats()
	// L1 (1s) expired several times over 2.5s; those misses must have hit L2, not
	// the origin — proving the two-tier bound (else origin would be >1).
	if st.L2Hits == 0 {
		t.Fatalf("expected L1 expiries to be absorbed by L2 (L2Hits>0), got 0")
	}
	rps := float64(served.Load()) / elapsed.Seconds()
	t.Logf("sustained: served=%d in %s (%.0f req/s in-proc) ⇒ origin_fetches=%d (%.4f QPS ≤ 1) l1_hits=%d l2_hits=%d",
		served.Load(), elapsed.Round(time.Millisecond), rps, of, originQPS, st.L1Hits, st.L2Hits)
}
