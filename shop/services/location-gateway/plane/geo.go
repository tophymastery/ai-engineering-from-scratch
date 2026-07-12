package plane

import (
	"fmt"
	"math"
)

// geo.go — the H3 res-7 geo primitive for the driver-telemetry geo index (D15).
//
// D15: "Live positions in Redis Cluster keyed by H3 res-7 cell (30 s TTL);
// dispatch kNN = order's cell + 6 neighbors." This sandbox has no vendored H3
// library — the repo's std-lib-only ethos (libs/sharding is "standard library
// only … zero attack surface"), and V-T4 (services/search-indexer/index/geo.go)
// and V-T12 (services/dispatch/match/geo.go) both model H3 with a FAITHFUL
// deterministic equal-angle bin at their resolution rather than adding a
// dependency. V-T13 does the same at res-7: a cell is the (lat,lng) grid square
// of side Res7DegLat. In the platform's SE-Asia cells (ID/VN/TH, near the
// equator) a degree of longitude ≈ a degree of latitude in km, so an equal-angle
// bin is a good geometric stand-in for a res-7 hex at these latitudes.
//
// The PROPERTIES that matter — every position maps to exactly one cell, kNN over
// (cell + expanding rings) returns the actually-nearest drivers, and the hottest
// physical geo key stays < 2% of writes after salting — are real code exercised
// for real by the tests in this package. Only the store engine (in-memory vs
// Redis Cluster) and wall-clock throughput are adapted, disclosed in
// VERIFICATION.md §V-T13.

// Res7DegLat is the side of an H3-res-7-equivalent cell in degrees. An H3 res-7
// hexagon has an average edge ~1.22 km (cell "diameter" ~2.4 km); 0.0111° at the
// equator is ~1.24 km, matching the res-7 edge scale (~6.9× finer than the
// Res5DegLat=0.1315 the search/dispatch slices use, i.e. two H3 resolutions).
const Res7DegLat = 0.0111

// metersPerDegLat is the length of one degree of latitude (WGS84 mean). Used to
// convert the cell grid to a metric lower bound for the kNN ring-stop rule.
const metersPerDegLat = 111320.0

// CellMeters is the approximate side of a res-7 cell in metres (~1.24 km).
const CellMeters = Res7DegLat * metersPerDegLat

// Cell is an H3-res-7-equivalent cell: the integer grid coordinates of a res-7
// bin. Deterministic and pure — the same lat/lng always yields the same cell.
type Cell struct {
	Lat int32 `json:"lat"`
	Lng int32 `json:"lng"`
}

// LatLngToCell maps a point to its res-7 cell. THE geo-index primitive: the geo
// key a driver position (or a kNN query point) lives under derives from this.
func LatLngToCell(lat, lng float64) Cell {
	return Cell{
		Lat: int32(math.Floor(lat / Res7DegLat)),
		Lng: int32(math.Floor(lng / Res7DegLat)),
	}
}

// Key is the stable string identity of a cell — the H3 res-7 geo key (D15). The
// physical Redis key salts this (salt.go) so a hot cell is spread across sub-keys.
func (c Cell) Key() string { return fmt.Sprintf("h7_%d_%d", c.Lat, c.Lng) }

// Ring returns the cells at Chebyshev distance exactly r from c (r=0 → {c}; r=1 →
// the 8-cell Moore neighbourhood; the D15 "cell + 6 neighbors" read fans out over
// Ring(0)+Ring(1)). kNN expands rings outward until the k-th candidate is provably
// closer than any unseen cell (see GeoStore.KNN).
func (c Cell) Ring(r int32) []Cell {
	if r == 0 {
		return []Cell{c}
	}
	var out []Cell
	for dx := -r; dx <= r; dx++ {
		for dy := -r; dy <= r; dy++ {
			if maxAbs(dx, dy) != r {
				continue // only the perimeter of the r-box
			}
			out = append(out, Cell{Lat: c.Lat + dx, Lng: c.Lng + dy})
		}
	}
	return out
}

func maxAbs(a, b int32) int32 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	if a > b {
		return a
	}
	return b
}

// HaversineM returns the great-circle distance between two lat/lng points in
// metres — the exact distance kNN ranks on (the equal-angle bin is only the index;
// ranking is true geodesic distance, so kNN correctness does not depend on the
// bin geometry).
func HaversineM(aLat, aLng, bLat, bLng float64) float64 {
	const earthR = 6371000.0
	rad := math.Pi / 180
	la1, la2 := aLat*rad, bLat*rad
	dLa := (bLat - aLat) * rad
	dLo := (bLng - aLng) * rad
	h := math.Sin(dLa/2)*math.Sin(dLa/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLo/2)*math.Sin(dLo/2)
	return 2 * earthR * math.Asin(math.Min(1, math.Sqrt(h)))
}
