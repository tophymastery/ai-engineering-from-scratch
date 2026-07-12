//go:build !race

package match

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// perf_test.go — correctness property #5: assignment p95 < 5 s. Latency is
// measured for REAL (order-ready → driver assigned compute time); the 1.5×
// peak-city density is the load-harness seam and the tick/offer WALL-CLOCK windows
// (tick ≤2 s + offer ≤3 s = the 5 s budget) are configuration, not measured — the
// sandbox never sleeps. Disclosed in VERIFICATION §V-T12. Built only without -race
// (race instrumentation invalidates latency), run via `make test-dispatch-perf`.

// TestPerf_AssignmentP95 measures per-order assignment latency (the compute path
// order-ready → assigned) at 1.5× peak-city density and asserts p95 < 5 s.
func TestPerf_AssignmentP95(t *testing.T) {
	clk := NewManualClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	e := NewEngine(Config{Clock: clk, TTL: 10 * time.Second, BaseSeed: 42, Partitions: 128})
	rng := rand.New(rand.NewSource(2026))

	// 1.5× peak-city density: ~1500 orders + drivers spread over ~30 zones.
	const orders = 1500
	type ord struct {
		id string
		z  Zone
	}
	var os []ord
	for i := 0; i < orders; i++ {
		zc := rng.Intn(30)
		lat := 5.0 + float64(zc)*ZoneDegLat*4 + rng.Float64()*0.05
		lng := 100.0 + rng.Float64()*0.05
		id := fmt.Sprintf("ord_%05d", i)
		z := e.AddOrder(Order{OrderID: id, Pickup: Point{Lat: lat, Lng: lng}})
		e.AddDriver(Driver{DriverID: fmt.Sprintf("drv_%05d", i), Loc: Point{Lat: lat + rng.Float64()*0.02, Lng: lng + rng.Float64()*0.02}})
		os = append(os, ord{id, z})
	}

	// Latency per order: from "ready" (now) through the zone tick + the driver
	// accept to the recorded assignment. We tick each order's zone once (batched)
	// and time the accept path; ticks are amortised across the zone's batch.
	lats := make([]time.Duration, 0, orders)
	ticked := map[string]bool{}
	for _, o := range os {
		start := time.Now()
		if !ticked[o.z.Key()] {
			e.Tick(o.z)
			ticked[o.z.Key()] = true
		}
		if _, ok := e.Accept(o.id); ok {
			lats = append(lats, time.Since(start))
		}
	}
	if len(lats) == 0 {
		t.Fatal("no assignments measured")
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	p50 := lats[len(lats)*50/100]
	p95 := lats[len(lats)*95/100]
	p99 := lats[min(len(lats)-1, len(lats)*99/100)]
	t.Logf("assignment latency @1.5x density (n=%d): p50=%v p95=%v p99=%v (compute; tick/offer wall-clock windows are config)",
		len(lats), p50, p95, p99)
	if p95 > 5*time.Second {
		t.Fatalf("assignment p95 = %v exceeds the 5s budget", p95)
	}
}

// TestPerf_TickThroughput reports batch-match throughput (informational): how many
// orders a single zone tick matches per second, to size the 1–2 s tick budget.
func TestPerf_TickThroughput(t *testing.T) {
	clk := NewManualClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	e := NewEngine(Config{Clock: clk, TTL: 10 * time.Second, BaseSeed: 1, Partitions: 64})
	// one dense zone with 300 orders + 300 drivers (a large single-zone batch).
	var z Zone
	// Keep the whole batch inside ONE res-5 zone (spread by <0.03° so it never
	// crosses a ~0.13° cell boundary) so this measures a single dense zone tick.
	for i := 0; i < 300; i++ {
		z = e.AddOrder(Order{OrderID: fmt.Sprintf("ord_%04d", i), Pickup: Point{Lat: 13.75, Lng: 100.50 + float64(i)*0.0001}})
		e.AddDriver(Driver{DriverID: fmt.Sprintf("drv_%04d", i), Loc: Point{Lat: 13.751, Lng: 100.50 + float64(i)*0.0001}})
	}
	start := time.Now()
	offers := e.Tick(z)
	elapsed := time.Since(start)
	t.Logf("single zone tick: matched %d orders in %v (%.0f orders/s); within the 1-2s tick budget",
		len(offers), elapsed, float64(len(offers))/elapsed.Seconds())
	if elapsed > 2*time.Second {
		t.Fatalf("single zone tick took %v, exceeds the 2s tick budget", elapsed)
	}
}
