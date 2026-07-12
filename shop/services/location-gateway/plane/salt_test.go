package plane

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

// salt_test.go — the headline write-distribution property (D15 / V-T13 criterion):
// under a realistic driver write distribution, the hottest physical H3 geo key
// receives < 2% of writes. Real write histogram off the REAL Update() write path
// (salted res-7 keys), no synthetic distribution.

// realisticFixture writes a spatially SKEWED driver population into the geo store:
// a handful of very hot "downtown" res-7 cells hold most drivers (Zipf-like), the
// rest are spread over the metro — the exact hot-cell shape D15 warns about ("one
// GEO key is itself a hot partition at 500k drivers"). Every driver emits the same
// number of writes, so the histogram measures the SPATIAL hot-key concentration
// after salting, nothing else.
func writeRealisticPopulation(g *GeoStore, drivers, writesPerDriver int, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	// Bangkok metro bbox.
	const (
		latLo, latHi = 13.60, 13.95
		lngLo, lngHi = 100.40, 100.75
	)
	// A few very hot downtown centres (Zipf weights): most drivers cluster here.
	hot := []struct{ lat, lng, w float64 }{
		{13.7460, 100.5340, 40}, // Siam/Ratchaprasong
		{13.7280, 100.5240, 22}, // Sathorn/Silom
		{13.7650, 100.5380, 14}, // Asok
		{13.7220, 100.4930, 9},  // Krung Thonburi
	}
	total := 0.0
	for _, h := range hot {
		total += h.w
	}
	for d := 0; d < drivers; d++ {
		id := fmt.Sprintf("drv_%07d", d)
		var lat, lng float64
		if rng.Float64() < 0.80 { // 80% of drivers sit in a hot centre
			pick := rng.Float64() * total
			acc := 0.0
			sel := hot[0]
			for _, h := range hot {
				acc += h.w
				if pick <= acc {
					sel = h
					break
				}
			}
			// tight ~1km spread inside the hot cell
			lat = sel.lat + (rng.Float64()-0.5)*0.012
			lng = sel.lng + (rng.Float64()-0.5)*0.012
		} else { // 20% spread across the whole metro
			lat = latLo + rng.Float64()*(latHi-latLo)
			lng = lngLo + rng.Float64()*(lngHi-lngLo)
		}
		for w := 0; w < writesPerDriver; w++ {
			// jitter each write slightly (a moving driver) but stay in-area
			jlat := lat + (rng.Float64()-0.5)*0.002
			jlng := lng + (rng.Float64()-0.5)*0.002
			g.Update(id, jlat, jlng, time.Unix(int64(w), 0).UTC())
		}
	}
}

// TestHottestGeoKeyUnderTwoPercent is the V-T13 headline: hottest H3 key < 2% of
// writes on a realistic skewed population.
func TestHottestGeoKeyUnderTwoPercent(t *testing.T) {
	g := NewGeoStore(NewManualClock(time.Unix(0, 0).UTC()), DefaultTTL)
	const drivers, writesPerDriver = 50000, 20
	writeRealisticPopulation(g, drivers, writesPerDriver, 2026)

	frac, key, hottest, total := g.HottestKeyFraction()
	perKey, _ := g.KeyHistogram()
	// Also report the hottest UNSALTED cell fraction, to show what salting bought.
	byCell := map[string]int64{}
	for k, v := range perKey {
		cell := k
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '#' {
				cell = k[:i]
				break
			}
		}
		byCell[cell] += v
	}
	var hottestCell int64
	for _, v := range byCell {
		if v > hottestCell {
			hottestCell = v
		}
	}
	cellFrac := float64(hottestCell) / float64(total)

	t.Logf("write histogram: drivers=%d writes=%d physical_keys=%d cells=%d",
		drivers, total, len(perKey), len(byCell))
	t.Logf("hottest UNSALTED cell = %d (%.3f%% of writes) — the D15 hot partition without salting",
		hottestCell, cellFrac*100)
	t.Logf("hottest SALTED key %q = %d (%.4f%% of writes) — after %d-way salt spread",
		key, hottest, frac*100, NumSalts)

	if frac >= 0.02 {
		t.Fatalf("hottest H3 key = %.4f%% of writes >= 2%% (D15 hot-key property FAILED)", frac*100)
	}
	if frac == 0 {
		t.Fatal("no writes recorded — histogram is empty")
	}
}

// TestSaltSpreadsDegenerateSingleCell is the WORST case: every driver in ONE res-7
// cell. Salting alone must still keep the hottest physical key < 2% of writes
// (1/NumSalts = 1.5625% ceiling), so the property is guaranteed even under maximal
// spatial concentration, not just the realistic skew above.
func TestSaltSpreadsDegenerateSingleCell(t *testing.T) {
	g := NewGeoStore(NewManualClock(time.Unix(0, 0).UTC()), DefaultTTL)
	const drivers, writesPerDriver = 20000, 10
	// All drivers inside a single res-7 cell (a ~1km box), one hot key without salt.
	for d := 0; d < drivers; d++ {
		id := fmt.Sprintf("drv_%07d", d)
		lat := 13.7460 + float64(d%7)*0.0001
		lng := 100.5340 + float64((d/7)%7)*0.0001
		for w := 0; w < writesPerDriver; w++ {
			g.Update(id, lat, lng, time.Unix(int64(w), 0).UTC())
		}
	}
	// Confirm it really is one cell.
	perKey, total := g.KeyHistogram()
	cells := map[string]bool{}
	for k := range perKey {
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '#' {
				cells[k[:i]] = true
				break
			}
		}
	}
	if len(cells) != 1 {
		t.Fatalf("fixture not single-cell: %d cells", len(cells))
	}
	frac, _, hottest, _ := g.HottestKeyFraction()
	t.Logf("degenerate single-cell: %d writes across %d salt keys — hottest=%d (%.4f%%), 1/%d ceiling=%.4f%%",
		total, len(perKey), hottest, frac*100, NumSalts, 100.0/float64(NumSalts))
	if frac >= 0.02 {
		t.Fatalf("degenerate hottest key = %.4f%% >= 2%% — %d salts insufficient", frac*100, NumSalts)
	}
	// Spread must be near-uniform: hottest within 1.5x of the mean salt bucket.
	mean := float64(total) / float64(NumSalts)
	if float64(hottest) > 1.5*mean {
		t.Fatalf("salt spread uneven: hottest %d > 1.5x mean %.0f", hottest, mean)
	}
}

var _ = math.Abs
