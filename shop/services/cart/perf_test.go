//go:build !race

// Perf latency numbers are only meaningful WITHOUT the race detector (race
// instrumentation inflates wall-clock ~10x and, combined with the single-writer
// in-memory SQLite connection, would report contended latencies that reflect the
// sandbox, not the code). So these run in a dedicated non-race pass
// (`make test-cart-perf`); the concurrency CORRECTNESS proof
// (TestConcurrentAddFixture, 100% stale writes → 412) lives in service_test.go
// and DOES run under -race.

package main

import (
	"fmt"
	"net/http"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// perf_test.go — the V-T7 perf test-criterion (cart ops p99 < 100 ms), measured
// HONESTLY at an adapted scale. Sustaining a literal 5k RPS for a
// statistically-meaningful window is not reachable in this sandbox (no cluster,
// in-memory SQLite serialised to one writer), so we measure:
//   (a) per-operation latency p50/p95/p99 for add / get / remove over thousands
//       of real requests through the full HTTP handler + snapshot + store path, and
//   (b) a concurrency BURST at a target fan-out to show the p99 holds under
//       contention.
// The measured numbers are printed and asserted against the 100 ms budget. The
// throughput adaptation (per-op + burst instead of a 5k-RPS soak) is disclosed
// here and in VERIFICATION.md §V-T7.

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p / 100 * float64(len(sorted)-1))
	return sorted[idx]
}

func summarize(name string, ds []time.Duration) (p50, p95, p99 time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p50 = percentile(ds, 50)
	p95 = percentile(ds, 95)
	p99 = percentile(ds, 99)
	fmt.Printf("  %-26s n=%d  p50=%v  p95=%v  p99=%v  max=%v\n",
		name, len(ds), p50.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond), ds[len(ds)-1].Round(time.Microsecond))
	return
}

// TestPerf_CartOps_P99 drives add / get / remove through the real handler and
// asserts each op's p99 is well under the 100 ms budget.
func TestPerf_CartOps_P99(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf in -short")
	}
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Perf", 100)

	// Create the cart, capture the rolling ETag.
	_, etag, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))

	const iters = 3000
	addLat := make([]time.Duration, 0, iters)
	getLat := make([]time.Duration, 0, iters)
	remLat := make([]time.Duration, 0, iters)
	// The catalog item is cached in cart's view after the first add, so each op is
	// pure HTTP+snapshot+store (no per-iteration catalog fetch) — the steady state.
	for i := 0; i < iters; i++ {
		// ADD the item (re-inserts the line removed at the end of the prior
		// iteration), carries the rolling ETag.
		t0 := time.Now()
		code, next, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag, addBody(tItem, tMerchant, 1))
		addLat = append(addLat, time.Since(t0))
		if code != 200 {
			t.Fatalf("perf add %d got %d", i, code)
		}
		etag = next

		// GET the cart.
		t1 := time.Now()
		code, _, _ = do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "")
		getLat = append(getLat, time.Since(t1))
		if code != 200 {
			t.Fatalf("perf get %d got %d", i, code)
		}

		// REMOVE the item, carries the ETag (empties the line for the next add).
		t2 := time.Now()
		code, next, _ = do(t, h, http.MethodDelete, "/v1/carts/"+tCart+"/items/"+tItem, etag, "")
		remLat = append(remLat, time.Since(t2))
		if code != 200 {
			t.Fatalf("perf remove %d got %d", i, code)
		}
		etag = next
	}
	fmt.Println("cart-ops latency (adapted scale — per-op through full HTTP+snapshot+store):")
	_, _, addP99 := summarize("POST /items (add)", addLat)
	_, _, getP99 := summarize("GET /carts/{id} (get)", getLat)
	_, _, remP99 := summarize("DELETE /items/{id} (remove)", remLat)

	const budget = 100 * time.Millisecond
	for name, p99 := range map[string]time.Duration{"add": addP99, "get": getP99, "remove": remP99} {
		if p99 > budget {
			t.Fatalf("cart %s p99 %v exceeds %v budget", name, p99, budget)
		}
	}
}

// TestPerf_ConcurrentBurst_P99 fires a burst of concurrent adds (target fan-out,
// each client on its own cart) and asserts the add p99 holds under the 100 ms
// budget even with the snapshot + store contended.
func TestPerf_ConcurrentBurst_P99(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf in -short")
	}
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Perf", 100)

	const clients = 64
	const perClient = 40
	all := make([]time.Duration, 0, clients*perClient)
	ch := make(chan []time.Duration, clients)
	var errs int64
	for c := 0; c < clients; c++ {
		go func(c int) {
			cartID := fmt.Sprintf("crt_perfburst%019d", c)
			local := make([]time.Duration, 0, perClient)
			var etag string
			for i := 0; i < perClient; i++ {
				t0 := time.Now()
				code, next, _ := do(t, h, http.MethodPost, "/v1/carts/"+cartID+"/items", etag, addBody(tItem, tMerchant, 1))
				local = append(local, time.Since(t0))
				if code != 200 {
					atomic.AddInt64(&errs, 1)
				}
				etag = next
			}
			ch <- local
		}(c)
	}
	for c := 0; c < clients; c++ {
		all = append(all, (<-ch)...)
	}
	if errs != 0 {
		t.Fatalf("burst had %d non-200 adds", errs)
	}
	fmt.Printf("concurrent burst (%d clients × %d adds = %d ops):\n", clients, perClient, len(all))
	_, _, p99 := summarize("POST /items under burst", all)
	if p99 > 100*time.Millisecond {
		t.Fatalf("burst add p99 %v exceeds 100ms", p99)
	}
}
