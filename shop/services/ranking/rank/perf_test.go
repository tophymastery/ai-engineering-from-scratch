//go:build !race

// Perf latency numbers are only meaningful WITHOUT the race detector (race
// instrumentation inflates wall-clock ~10× and would report the sandbox, not the
// code). These run in a dedicated non-race pass (`make test-ranking-perf`); the
// CORRECTNESS properties (ML-vs-static, event-fed features, auto-fallback timing
// + availability, exactly-once) live in the other _test.go files and DO run under
// -race.
//
// Scale note (disclosed in VERIFICATION.md §V-T5): the re-rank LATENCY (< 50 ms
// p99 for a top-500 → top-50 re-rank) is the real property and is measured
// genuinely here per-op; only sustained cluster-scale QPS is out of reach in this
// sandbox and is not claimed.

package rank

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// seedCandidates builds a realistic top-500 retrieval set with feature vectors
// loaded (the worst case: every candidate has a feature lookup).
func seedCandidates(feats *FeatureStore, n int) []Candidate {
	cands := make([]Candidate, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("mer_%04d", i)
		feats.Apply(id, SignalImpression, float64(50+i%200))
		feats.Apply(id, SignalClick, float64(i%40))
		feats.Apply(id, SignalOrder, float64(i%100))
		cands[i] = Candidate{
			StoreID:     id,
			Name:        fmt.Sprintf("Store %d", i),
			Rating:      3.0 + float64(i%20)*0.1,
			DistanceM:   100 + (i*7)%5000,
			Open:        true,
			DeliveryFee: Money{Amount: 1500, Currency: "THB"},
			ETAMinutes:  15,
		}
	}
	return cands
}

// TestPerf_ReRankP99 is the D17 latency property: re-rank (top-500 → top-50) adds
// < 50 ms p99. It measures the real per-op Rank latency over many iterations on a
// 500-candidate set with the ML model active and features loaded.
func TestPerf_ReRankP99(t *testing.T) {
	if testing.Short() {
		t.Skip("perf")
	}
	feats := NewFeatureStore()
	cands := seedCandidates(feats, 500)
	r := NewRanker(feats, Options{})

	// Warm up.
	for i := 0; i < 1000; i++ {
		_, _ = r.Rank(context.Background(), cands, DefaultTopK, true)
	}

	const iters = 20000
	lat := make([]time.Duration, iters)
	start := time.Now()
	for i := 0; i < iters; i++ {
		s := time.Now()
		out, usedML := r.Rank(context.Background(), cands, DefaultTopK, true)
		lat[i] = time.Since(s)
		if !usedML || len(out) != DefaultTopK {
			t.Fatalf("iter %d: usedML=%v len=%d (want ML + %d)", i, usedML, len(out), DefaultTopK)
		}
	}
	total := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50 := lat[iters*50/100]
	p95 := lat[iters*95/100]
	p99 := lat[iters*99/100]
	ops := float64(iters) / total.Seconds()
	t.Logf("re-rank top-500->top-50 (ML, features loaded, %d ops): p50=%v p95=%v p99=%v; in-process throughput=%.0f rerank/s", iters, p50, p95, p99, ops)
	if p99 >= 50*time.Millisecond {
		t.Fatalf("re-rank p99 %v ≥ 50ms (D17 re-rank budget FAILED)", p99)
	}

	// Static path is even cheaper — sanity check it is also well under budget.
	sl := make([]time.Duration, 5000)
	for i := range sl {
		s := time.Now()
		_, _ = r.Rank(context.Background(), cands, DefaultTopK, false)
		sl[i] = time.Since(s)
	}
	sort.Slice(sl, func(i, j int) bool { return sl[i] < sl[j] })
	t.Logf("static-fallback re-rank p99=%v (shed-ladder L1 path)", sl[len(sl)*99/100])
}
