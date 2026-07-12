package match

import (
	"math/rand"
	"testing"
)

// TestZoneDeterministic: the same point always maps to the same zone, and points
// within one res-5 cell share a zone (the unit of single-writer ownership).
func TestZoneDeterministic(t *testing.T) {
	p := Point{Lat: 13.7563, Lng: 100.5018}
	z1, z2 := ZoneFor(p), ZoneFor(p)
	if z1 != z2 {
		t.Fatalf("zone not deterministic: %v vs %v", z1, z2)
	}
	// a point a few metres away is the same zone; a point a full cell away is not.
	near := Point{Lat: p.Lat + 0.001, Lng: p.Lng + 0.001}
	if ZoneFor(near) != z1 {
		t.Fatalf("nearby point should share the zone")
	}
	far := Point{Lat: p.Lat + ZoneDegLat*2, Lng: p.Lng}
	if ZoneFor(far) == z1 {
		t.Fatal("a point two cells away must be a different zone")
	}
}

// TestZonePinnedToOnePartition is the D13 "Kafka partition per zone" invariant
// expressed in code: a zone hashes to exactly ONE partition, stably across calls,
// so one consumer owns the zone ⇒ a single writer per zone. (The Kafka topology
// itself is render-only in-sandbox; this pinning is the real, tested half.)
func TestZonePinnedToOnePartition(t *testing.T) {
	const nParts = 128
	rng := rand.New(rand.NewSource(5))
	counts := make([]int, nParts)
	seen := map[string]int{}
	for i := 0; i < 5000; i++ {
		z := ZoneFor(Point{Lat: 5 + rng.Float64()*30, Lng: 95 + rng.Float64()*15})
		p := z.Partition(nParts)
		if p < 0 || p >= nParts {
			t.Fatalf("partition %d out of range", p)
		}
		if prev, ok := seen[z.Key()]; ok && prev != p {
			t.Fatalf("zone %s mapped to two partitions %d and %d", z.Key(), prev, p)
		}
		seen[z.Key()] = p
		counts[p]++
	}
	// sanity: the hash spreads zones across many partitions (not all in one).
	used := 0
	for _, c := range counts {
		if c > 0 {
			used++
		}
	}
	if used < nParts/2 {
		t.Fatalf("partition spread too narrow: only %d/%d partitions used", used, nParts)
	}
	t.Logf("zone→partition pinning stable across 5000 lookups; %d/%d partitions used", used, nParts)
}

// TestETAMatchesMapSimFormula: the local ETA twin reproduces map-sim's CAR formula
// (haversine × 1.3 ÷ 8.333 m/s), so an in-process replay equals a real map-sim
// call. Spot-checks against a hand computation.
func TestETAMatchesMapSimFormula(t *testing.T) {
	from := Point{Lat: 13.75, Lng: 100.50}
	to := Point{Lat: 13.76, Lng: 100.50}
	got := ETASeconds(from, to)
	// ~1.11 km straight; ×1.3 ≈ 1.446 km; ÷8.333 ≈ 174 s.
	if got < 150 || got > 200 {
		t.Fatalf("ETA %ds outside expected ~174s band", got)
	}
	if ETASeconds(from, from) != 0 {
		t.Fatalf("ETA to self must be 0, got %d", ETASeconds(from, from))
	}
}
