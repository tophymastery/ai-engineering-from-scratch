//go:build !race

package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// perf_test.go — V-T8 latency criterion: quote p99 < 300 ms. Built WITHOUT -race
// (race instrumentation inflates latency and invalidates the number). Measures
// the REAL per-quote path: HTTP decode → deterministic engine → HMAC sign →
// Redis-like put. Throughput adaptation is disclosed in VERIFICATION.md §V-T8:
// a literal sustained 10k RPS is unreachable in this single-process sandbox, so
// the budget is proven by measured per-op p99 + a concurrency burst, not a soak.

func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * q)
	return cp[idx]
}

// TestPerf_QuoteP99 measures single-client per-quote p99 over 3000 quotes.
func TestPerf_QuoteP99(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped under -short")
	}
	s, _, _ := newTestServer(t)
	h := s.mux()
	body := createBody(tCart, 40000, "THB", "LUNCH25", true)

	const N = 3000
	lat := make([]time.Duration, 0, N)
	for i := 0; i < N; i++ {
		req := httptest.NewRequest("POST", "/v1/quotes", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		t0 := time.Now()
		h.ServeHTTP(rec, req)
		lat = append(lat, time.Since(t0))
		if rec.Code != 201 {
			t.Fatalf("quote %d -> %d", i, rec.Code)
		}
	}
	p50, p99, mx := percentile(lat, 0.50), percentile(lat, 0.99), percentile(lat, 1.0)
	t.Logf("quote latency over %d: p50=%v p99=%v max=%v (budget p99 < 300ms)", N, p50, p99, mx)
	if p99 >= 300*time.Millisecond {
		t.Fatalf("quote p99 = %v exceeds 300ms budget", p99)
	}
}

// TestPerf_QuoteBurst runs a concurrent burst (64 clients × 60 quotes) and
// asserts p99 stays under budget — the in-sandbox stand-in for sustained RPS.
func TestPerf_QuoteBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped under -short")
	}
	s, _, _ := newTestServer(t)
	h := s.mux()
	body := createBody(tCart, 40000, "THB", "LUNCH25", true)

	const clients, perClient = 64, 60
	lat := make([]time.Duration, clients*perClient)
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < perClient; i++ {
				req := httptest.NewRequest("POST", "/v1/quotes", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				t0 := time.Now()
				h.ServeHTTP(rec, req)
				lat[c*perClient+i] = time.Since(t0)
				if rec.Code != 201 {
					panic(fmt.Sprintf("burst quote -> %d", rec.Code))
				}
			}
		}(c)
	}
	wg.Wait()
	p99 := percentile(lat, 0.99)
	t.Logf("burst %d clients × %d: p99=%v max=%v (budget < 300ms)", clients, perClient, p99, percentile(lat, 1.0))
	if p99 >= 300*time.Millisecond {
		t.Fatalf("burst quote p99 = %v exceeds 300ms budget", p99)
	}
}

// TestPerf_CheckoutP99 measures the verify+persist checkout path p99.
func TestPerf_CheckoutP99(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped under -short")
	}
	s, _, _ := newTestServer(t)
	h := s.mux()

	const N = 2000
	lat := make([]time.Duration, 0, N)
	for i := 0; i < N; i++ {
		_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(fmt.Sprintf("crt_%d", i), 40000, "THB", "LUNCH25", true))
		qb, _ := json.Marshal(q)
		req := httptest.NewRequest("POST", "/v1/quotes/"+q.QuoteID+":checkout", strings.NewReader(string(qb)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		t0 := time.Now()
		h.ServeHTTP(rec, req)
		lat = append(lat, time.Since(t0))
		if rec.Code != 200 {
			t.Fatalf("checkout %d -> %d", i, rec.Code)
		}
	}
	p99 := percentile(lat, 0.99)
	t.Logf("checkout latency over %d: p50=%v p99=%v (verify+persist)", N, percentile(lat, 0.50), p99)
	if p99 >= 300*time.Millisecond {
		t.Fatalf("checkout p99 = %v exceeds 300ms budget", p99)
	}
}
