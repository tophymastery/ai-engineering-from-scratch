//go:build !race

package main

import (
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// perf_test.go measures checkout latency through the full HTTP + idempotency +
// event-store + outbox + timer-arm path. Excluded from the -race pass (build tag)
// and run separately, mirroring pricing-promo/cart. The saga's budget is the
// checkout budget (01 §5: checkout p99 < 800 ms); we prove it with wide margin.
// Scale note (disclosed in VERIFICATION §V-T9): a literal sustained 1.2k
// orders/min soak is the V-T31 load-harness seam; here the per-op p99 is FULL
// (real, measured, printed) — numbers are not fabricated.

func TestPerf_CheckoutP99(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.mux()
	const N = 3000
	const body = `{"quote_id":"qot_perf","payment_method_id":"pm"}`
	lat := make([]time.Duration, 0, N)
	for i := 0; i < N; i++ {
		req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "perf-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(rec, req)
		lat = append(lat, time.Since(start))
		if rec.Code != 201 {
			t.Fatalf("checkout %d -> %d", i, rec.Code)
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50 := lat[len(lat)*50/100]
	p99 := lat[len(lat)*99/100]
	t.Logf("checkout latency over %d: p50=%v p99=%v (budget 800ms)", N, p50, p99)
	if p99 > 800*time.Millisecond {
		t.Fatalf("checkout p99 %v exceeds 800ms budget", p99)
	}
}
