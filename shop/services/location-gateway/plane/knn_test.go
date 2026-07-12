package plane

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// knn_test.go — kNN CORRECTNESS: the geo index's ring-expanding kNN returns the
// actually-nearest drivers, verified against a brute-force over the same fixture.
// This is the read contract dispatch consumes; if it were approximate, dispatch
// would offer the wrong driver.

// bruteKNN is the reference: exact k-nearest by scanning every live driver.
func bruteKNN(query [2]float64, drivers map[string][2]float64, k int) []string {
	type dd struct {
		id string
		d  float64
	}
	all := make([]dd, 0, len(drivers))
	for id, p := range drivers {
		all = append(all, dd{id, HaversineM(query[0], query[1], p[0], p[1])})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].d != all[j].d {
			return all[i].d < all[j].d
		}
		return all[i].id < all[j].id
	})
	if len(all) > k {
		all = all[:k]
	}
	out := make([]string, len(all))
	for i, x := range all {
		out[i] = x.id
	}
	return out
}

func knnIDs(ns []Neighbor) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.DriverID
	}
	return out
}

// TestKNNMatchesBruteForce runs many random fixtures + queries and asserts the
// geo index's kNN top-k is byte-identical to brute force (same ids, same order).
func TestKNNMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const trials = 400
	mismatches := 0
	for tr := 0; tr < trials; tr++ {
		g := NewGeoStore(NewManualClock(time.Unix(0, 0).UTC()), DefaultTTL)
		n := 20 + rng.Intn(300)
		drivers := make(map[string][2]float64, n)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("drv_%d_%04d", tr, i)
			lat := 13.60 + rng.Float64()*0.35
			lng := 100.40 + rng.Float64()*0.35
			drivers[id] = [2]float64{lat, lng}
			g.Update(id, lat, lng, time.Unix(0, 0).UTC())
		}
		q := [2]float64{13.60 + rng.Float64()*0.35, 100.40 + rng.Float64()*0.35}
		k := 1 + rng.Intn(15)

		got := knnIDs(g.KNN(q[0], q[1], k))
		want := bruteKNN(q, drivers, k)
		if !equalIDs(got, want) {
			mismatches++
			if mismatches <= 3 {
				t.Errorf("trial %d k=%d n=%d: kNN != brute\n got=%v\nwant=%v", tr, k, n, got, want)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d/%d trials mismatched brute force", mismatches, trials)
	}
	t.Logf("kNN correctness: %d/%d random fixtures match brute force EXACTLY (ids+order)", trials, trials)
}

// TestKNNRespectsTTL: expired positions (past the 30 s window) are not returned.
func TestKNNRespectsTTL(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	g := NewGeoStore(clk, DefaultTTL)
	g.Update("drv_stale", 13.75, 100.53, time.Unix(0, 0).UTC())
	clk.Advance(31 * time.Second) // past the 30 s TTL
	g.Update("drv_fresh", 13.7502, 100.5302, time.Unix(31, 0).UTC())

	ns := g.KNN(13.75, 100.53, 5)
	if len(ns) != 1 || ns[0].DriverID != "drv_fresh" {
		t.Fatalf("TTL not enforced: want only drv_fresh, got %v", knnIDs(ns))
	}
	if lc := g.LiveCount(); lc != 1 {
		t.Fatalf("live count want 1 (drv_stale expired), got %d", lc)
	}
	t.Logf("TTL: stale position (age 31s > 30s) excluded from kNN + live count")
}

// TestKNNDriverMovesCells: when a driver moves to a new res-7 cell, only its new
// position is live (no stale duplicate in the old cell).
func TestKNNDriverMovesCells(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	g := NewGeoStore(clk, DefaultTTL)
	g.Update("drv_move", 13.7000, 100.5000, time.Unix(0, 0).UTC())
	g.Update("drv_move", 13.8000, 100.6000, time.Unix(1, 0).UTC()) // far cell
	if lc := g.LiveCount(); lc != 1 {
		t.Fatalf("moved driver double-counted: live=%d want 1", lc)
	}
	// Querying near the OLD spot must not find it (it moved away).
	near := g.KNN(13.7000, 100.5000, 1)
	if len(near) != 1 {
		t.Fatalf("want the single live driver, got %d", len(near))
	}
	// Its reported cell must be the NEW cell.
	if near[0].H3Cell != LatLngToCell(13.8000, 100.6000).Key() {
		t.Fatalf("stale cell for moved driver: %s", near[0].H3Cell)
	}
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
