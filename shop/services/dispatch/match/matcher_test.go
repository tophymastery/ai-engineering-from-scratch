package match

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// skewedDataset builds the D13 "skewed" batch: `pairs` independent contested
// regions, each a classic per-order-greedy trap. In each region driver B sits ON
// TOP of order Q (Q needs B badly) but is ALSO marginally closer to order P; a
// per-order greedy that processes P first grabs B, stranding Q on the far driver A
// — a large, structural sub-optimality. The regions are 10° apart so they never
// interact (cross-region ETA is enormous), so the trap compounds cleanly. This is
// exactly the "greedy concentrates offers on the same top drivers and degrades
// when supply is scarce" failure D13's batch matcher is designed to beat.
func skewedDataset(pairs int) ([]Order, []Driver) {
	var orders []Order
	var drivers []Driver
	for k := 0; k < pairs; k++ {
		off := float64(k) * 10.0 // fully separate regions in latitude
		lng := 100.0
		// P id ..a sorts before Q id ..b, so greedy processes P first and grabs B.
		orders = append(orders,
			Order{OrderID: fmt.Sprintf("ord_r%04d_a", k), Pickup: Point{Lat: off + 0.011, Lng: lng}}, // P
			Order{OrderID: fmt.Sprintf("ord_r%04d_b", k), Pickup: Point{Lat: off + 0.020, Lng: lng}}, // Q (on top of B)
		)
		drivers = append(drivers,
			Driver{DriverID: fmt.Sprintf("drv_r%04d_a", k), Loc: Point{Lat: off + 0.000, Lng: lng}}, // A (far)
			Driver{DriverID: fmt.Sprintf("drv_r%04d_b", k), Loc: Point{Lat: off + 0.020, Lng: lng}}, // B (contested)
		)
	}
	return orders, drivers
}

// TestBatchBeatsGreedyByTenPercent is correctness property #4: on the skewed
// dataset the greedy-with-swaps batch matcher's sum-of-pickup-ETA is ≥10% lower
// than the greedy baseline. Real datasets, real map-sim-twin ETAs, real matchers.
func TestBatchBeatsGreedyByTenPercent(t *testing.T) {
	orders, drivers := skewedDataset(64)

	base := GreedyBaseline(orders, drivers, ETASeconds)
	batch := matchWithSeed(orders, drivers, ETASeconds, 1)

	if len(base) != len(orders) || len(batch) != len(orders) {
		t.Fatalf("both matchers must assign all %d orders: baseline=%d batch=%d", len(orders), len(base), len(batch))
	}
	gTot := TotalETA(base)
	bTot := TotalETA(batch)
	improvement := float64(gTot-bTot) / float64(gTot)
	t.Logf("sum-of-pickup-ETA: greedy baseline=%ds, greedy-with-swaps=%ds, improvement=%.1f%%",
		gTot, bTot, improvement*100)
	if improvement < 0.10 {
		t.Fatalf("batch matcher only %.1f%% better than greedy baseline, want >=10%%", improvement*100)
	}
}

// bruteForceOptimal computes the true minimum-total assignment by trying every
// permutation (n! — only for tiny n) to validate the local search reaches optimal.
func bruteForceOptimal(orders []Order, drivers []Driver, eta ETAFunc) int {
	os := sortedOrders(orders)
	ds := sortedDrivers(drivers)
	n := len(os)
	perm := make([]int, len(ds))
	for i := range perm {
		perm[i] = i
	}
	best := math.MaxInt
	var rec func(k int)
	rec = func(k int) {
		if k == n {
			tot := 0
			for i := 0; i < n; i++ {
				tot += eta(ds[perm[i]].Loc, os[i].Pickup)
			}
			if tot < best {
				best = tot
			}
			return
		}
		for i := k; i < len(perm); i++ {
			perm[k], perm[i] = perm[i], perm[k]
			rec(k + 1)
			perm[k], perm[i] = perm[i], perm[k]
		}
	}
	rec(0)
	return best
}

// TestMatchReachesOptimalSmall validates the greedy-with-swaps local search finds
// the true optimum on small batches (D13: "Hungarian for small batches" — we reach
// the same optimum via seeded-restart 2-opt).
func TestMatchReachesOptimalSmall(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for trial := 0; trial < 40; trial++ {
		var orders []Order
		var drivers []Driver
		n := 6
		for i := 0; i < n; i++ {
			orders = append(orders, Order{OrderID: fmt.Sprintf("ord_%02d", i), Pickup: Point{Lat: 13 + rng.Float64()*0.3, Lng: 100 + rng.Float64()*0.3}})
			drivers = append(drivers, Driver{DriverID: fmt.Sprintf("drv_%02d", i), Loc: Point{Lat: 13 + rng.Float64()*0.3, Lng: 100 + rng.Float64()*0.3}})
		}
		got := TotalETA(matchWithSeed(orders, drivers, ETASeconds, int64(trial)))
		opt := bruteForceOptimal(orders, drivers, ETASeconds)
		if got != opt {
			t.Fatalf("trial %d: matcher total=%d, optimal=%d (local search did not reach optimum)", trial, got, opt)
		}
	}
}

// TestMatchDeterministic proves the matcher is byte-identical across runs for the
// same inputs+seed (the basis of snapshot replay), and that input ORDER does not
// change the result (it sorts internally).
func TestMatchDeterministic(t *testing.T) {
	orders, drivers := skewedDataset(20)
	want := Canonical(matchWithSeed(orders, drivers, ETASeconds, 7))
	for i := 0; i < 50; i++ {
		// shuffle inputs each run; the matcher must still produce the identical result.
		ro := append([]Order(nil), orders...)
		rd := append([]Driver(nil), drivers...)
		rng := rand.New(rand.NewSource(int64(i)))
		rng.Shuffle(len(ro), func(a, b int) { ro[a], ro[b] = ro[b], ro[a] })
		rng.Shuffle(len(rd), func(a, b int) { rd[a], rd[b] = rd[b], rd[a] })
		got := Canonical(matchWithSeed(ro, rd, ETASeconds, 7))
		if got != want {
			t.Fatalf("run %d not deterministic:\n got=%s\nwant=%s", i, got, want)
		}
	}
}

// TestMatchScarcity: with fewer drivers than orders, the batch assigns exactly the
// number of drivers and every driver is used exactly once.
func TestMatchScarcity(t *testing.T) {
	var orders []Order
	for i := 0; i < 10; i++ {
		orders = append(orders, Order{OrderID: fmt.Sprintf("ord_%02d", i), Pickup: Point{Lat: 13.75, Lng: 100.5 + float64(i)*0.01}})
	}
	drivers := []Driver{
		{DriverID: "drv_a", Loc: Point{Lat: 13.75, Lng: 100.50}},
		{DriverID: "drv_b", Loc: Point{Lat: 13.75, Lng: 100.55}},
		{DriverID: "drv_c", Loc: Point{Lat: 13.75, Lng: 100.59}},
	}
	as := matchWithSeed(orders, drivers, ETASeconds, 3)
	if len(as) != 3 {
		t.Fatalf("scarcity: assigned %d, want 3 (one per driver)", len(as))
	}
	seen := map[string]bool{}
	for _, a := range as {
		if seen[a.DriverID] {
			t.Fatalf("driver %s assigned twice", a.DriverID)
		}
		seen[a.DriverID] = true
	}
}
