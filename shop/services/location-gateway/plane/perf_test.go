//go:build !race

package plane

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// perf_test.go — the V-T13 latency criteria: kNN p99 < 10 ms and gateway
// per-message ingest p99 < 5 ms. LATENCY is measured for REAL (the compute path,
// printed); the WRITE THROUGHPUT the criteria cite (200k writes/s for kNN, the
// 300k msg/s 1 h sustained burst for ingest) is the V-T31/V-T32 load-harness seam,
// not wall-clock reproduced here — the sandbox never sleeps and has no Kafka.
// Disclosed in VERIFICATION §V-T13. Built only without -race (race instrumentation
// invalidates latency); run via `make test-location-perf`.

func pctl(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * p)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// TestPerf_KNNp99 loads a realistic 200k-driver population into the geo index (the
// "at 200k writes/s" scale as a population, throughput adapted) and measures kNN
// query latency p99 over many random queries.
func TestPerf_KNNp99(t *testing.T) {
	g := NewGeoStore(NewManualClock(time.Unix(0, 0).UTC()), DefaultTTL)
	rng := rand.New(rand.NewSource(2026))
	const drivers = 200_000
	// Realistic metro spread: 200k drivers across the whole ~40km Bangkok metro
	// (not piled into one 5km box — that would be ~8000 drivers/km², absurd). A
	// moderate hot-centre skew keeps a few cells busy (the D15 hot partition) while
	// per-cell density stays plausible (~hundreds/cell), which is the density kNN
	// actually runs against in production.
	for d := 0; d < drivers; d++ {
		id := fmt.Sprintf("drv_%07d", d)
		var lat, lng float64
		if rng.Float64() < 0.55 { // 55% clustered around a few downtown centres
			cx := 13.700 + rng.Float64()*0.20
			cy := 100.45 + rng.Float64()*0.20
			lat = cx + (rng.Float64()-0.5)*0.04
			lng = cy + (rng.Float64()-0.5)*0.04
		} else { // 45% spread across the full metro
			lat = 13.60 + rng.Float64()*0.35
			lng = 100.40 + rng.Float64()*0.35
		}
		g.Update(id, lat, lng, time.Unix(0, 0).UTC())
	}

	const queries = 5000
	lats := make([]time.Duration, 0, queries)
	for q := 0; q < queries; q++ {
		// dispatch kNN queries are order pickup points spread across the metro
		qlat := 13.60 + rng.Float64()*0.35
		qlng := 100.40 + rng.Float64()*0.35
		st := time.Now()
		ns := g.KNN(qlat, qlng, 10) // dispatch asks for ~10 nearest
		el := time.Since(st)
		if len(ns) == 0 {
			t.Fatalf("empty kNN at query %d", q)
		}
		lats = append(lats, el)
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	p50, p95, p99 := pctl(lats, 0.50), pctl(lats, 0.95), pctl(lats, 0.99)
	t.Logf("kNN latency (n=%d drivers, %d queries, k=10): p50=%v p95=%v p99=%v (budget p99<10ms)",
		drivers, queries, p50, p95, p99)
	if p99 > 10*time.Millisecond {
		t.Fatalf("kNN p99 = %v exceeds the 10ms budget", p99)
	}
}

// TestPerf_IngestP99 measures the gateway per-message ingest latency (the Push
// hot path — buffer append, no auth, no network) over a sustained burst and
// asserts p99 < 5 ms, with zero produce errors on the 100 ms flushes.
func TestPerf_IngestP99(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	h, _, sink := newTestHub(clk)

	const streams = 2000
	sts := make([]*Stream, streams)
	for c := 0; c < streams; c++ {
		s, err := h.Open(fmt.Sprintf("conn-%05d", c), fmt.Sprintf("tok:%05d", c))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		sts[c] = s
	}

	const perStream = 300 // sustained burst: 600k messages total
	lats := make([]time.Duration, 0, streams*perStream)
	for i := 0; i < perStream; i++ {
		for _, s := range sts {
			st := time.Now()
			_ = s.Push(Frame{Lat: 13.75, Lng: 100.53, RecordedAt: clk.Now()})
			lats = append(lats, time.Since(st))
		}
		// flush a 100 ms window periodically
		clk.Advance(100 * time.Millisecond)
		h.Flush(false)
	}

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	p50, p95, p99 := pctl(lats, 0.50), pctl(lats, 0.95), pctl(lats, 0.99)
	total := streams * perStream
	t.Logf("gateway ingest latency (%d streams x %d = %d msgs): p50=%v p95=%v p99=%v (budget p99<5ms)",
		streams, perStream, total, p50, p95, p99)
	if p99 > 5*time.Millisecond {
		t.Fatalf("ingest p99 = %v exceeds the 5ms budget", p99)
	}
	if h.ProduceErrors() != 0 {
		t.Fatalf("produce errors during burst: %d (want 0)", h.ProduceErrors())
	}
	if int(h.Produced()) != sink.positions || h.Produced() == 0 {
		t.Fatalf("produced %d != sink %d", h.Produced(), sink.positions)
	}
	t.Logf("sustained burst: %d messages, %d produced, 0 produce errors", total, h.Produced())
}
