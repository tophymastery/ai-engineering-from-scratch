// Package index is the in-process search read model for the V-T4 Search & browse
// slice (implements D17 + D11). It is a FAITHFUL in-memory model of the
// production topology from D17 ("per-cell OpenSearch, index per country, shard
// routing by H3 res-5 ⇒ a query touches 1–2 shards; rating aggregates debounced;
// bulk-index pipeline with backpressure on dedicated ingest nodes") and D11
// (merchant fan-out topics use salted keys `merchant_id#(0..15)`, per-salt
// ordering, LWW projection).
//
// This sandbox has no OpenSearch and no Docker daemon, so the inverted index +
// shard router live in this package as plain Go. Everything that is a CORRECTNESS
// property — H3-res-5 shard routing (≤2 shards / geo query), salt balance
// (hottest partition < 2× mean), rating debounce (≤1 update/merchant/5 min),
// freshness lag, and bulk-index backpressure keeping feed latency stable — is
// real code exercised for real by the tests in this package. Only the store
// engine (in-memory vs OpenSearch) and wall-clock throughput are adapted, and
// both adaptations are disclosed in VERIFICATION.md §V-T4.
package index

import "math"

// --- H3 res-5 geo routing (D17) ---
//
// H3 res-5 hexagons have an average edge ~8.5 km (cell "diameter" ~15 km). No
// pure-Go H3 library is vendorable under the repo's std-lib-only ethos
// (libs/sharding is "standard library only … zero attack surface"), so we model
// res-5 with a FAITHFUL deterministic equal-angle bin at res-5 scale: a cell is
// the (lat,lng) grid square of side Res5DegLat. In the platform's SE-Asia cells
// (ID/VN/TH, near the equator) a degree of longitude ≈ a degree of latitude in
// km, so an equal-angle bin is a good geometric stand-in for a res-5 hex at these
// latitudes. The routing PROPERTY it must preserve — a bounded-radius geo query
// touches ≤2 shards ≥99% of the time — is measured for real in geo_test.go.

// Res5DegLat is the side of a res-5 cell in degrees (~14.6 km at the equator,
// matching an H3 res-5 hexagon's ~15 km cell diameter).
const Res5DegLat = 0.1315

// Cell is an H3-res-5-equivalent cell: the integer grid coordinates of a res-5
// bin. Deterministic and pure — the same lat/lng always yields the same cell.
type Cell struct {
	Lat int32
	Lng int32
}

// floorDiv is integer floor division that is correct for negatives (Go's / and %
// truncate toward zero, which would put the southern/western hemispheres on the
// wrong cell boundary).
func floorDiv(a, b int32) int32 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// LatLngToCell maps a point to its res-5 cell. THE geo routing primitive: the
// shard a document (or a query point) belongs to derives from this.
func LatLngToCell(lat, lng float64) Cell {
	return Cell{
		Lat: int32(math.Floor(lat / Res5DegLat)),
		Lng: int32(math.Floor(lng / Res5DegLat)),
	}
}

// --- shard routing ---
//
// A country index has NumShards primary shards (D17: the largest cell needs
// ~24 data nodes, so we model a 24-shard country index). The shard a cell routes
// to is chosen to be SPATIALLY CONTIGUOUS: res-5 cells are grouped into coarse
// "shard tiles" of ShardTileCells×ShardTileCells res-5 cells, and every cell in a
// shard tile routes to the same shard. Contiguity is what makes a bounded-radius
// geo query touch ≤2 shards: the query's covering cells almost always fall inside
// one shard tile, and when they straddle a tile edge they touch exactly two.
// (This is the routing OpenSearch achieves with a geo-aware routing key; here it
// is explicit and measured.)

// NumShards is the primary-shard count of a country index (D17 ~24 data nodes).
const NumShards = 24

// ShardTileCells is the side of a shard tile in res-5 cells. A shard tile is then
// ~1.7° (~190 km) on a side — large vs a delivery-radius query (~5 km ≈ 0.36
// cells), so a query's covering cells sit inside one tile with high probability
// and straddle at most two tile edges.
const ShardTileCells = 13

// shardStride spreads adjacent shard tiles across distinct shards. Coprime with
// NumShards (gcd(7,24)=1) so a row of shard tiles cycles through all 24 shards
// rather than aliasing onto a few.
const shardStride = 7

// CellToShard maps a res-5 cell to its primary shard in [0, NumShards). All res-5
// cells within one ShardTileCells×ShardTileCells shard tile share a shard
// (spatial contiguity), so a bounded geo query touches few shards.
func CellToShard(c Cell) int {
	superLat := floorDiv(c.Lat, ShardTileCells)
	superLng := floorDiv(c.Lng, ShardTileCells)
	// Mix into [0,NumShards) preserving tile adjacency (neighbouring tiles land
	// on neighbouring-ish shards, and — critically — a 2-tile straddle yields
	// exactly 2 distinct shards).
	v := superLat*shardStride + superLng
	m := v % NumShards
	if m < 0 {
		m += NumShards
	}
	return int(m)
}

// ShardForPoint routes a query/document point straight to its shard.
func ShardForPoint(lat, lng float64) int { return CellToShard(LatLngToCell(lat, lng)) }

// --- geo query coverage ---

// metersPerDegLat is the length of one degree of latitude (WGS84 mean). Used to
// convert a query radius in metres to a cell window.
const metersPerDegLat = 111320.0

// ShardsForQuery returns the DISTINCT shards a geo query (a point + radius in
// metres) must touch: the shards owning every res-5 cell whose square intersects
// the query disk. This is exactly what an OpenSearch geo_distance query fans out
// to, so counting the distinct shards here measures the D17 "a query touches 1–2
// shards" property for real.
func ShardsForQuery(lat, lng, radiusM float64) []int {
	// Convert the radius to a cell window. Longitude degrees shrink by cos(lat);
	// guard the poles (irrelevant for SE-Asia cells but keeps the math total).
	dLat := radiusM / metersPerDegLat
	cosLat := math.Cos(lat * math.Pi / 180)
	if cosLat < 1e-6 {
		cosLat = 1e-6
	}
	dLng := radiusM / (metersPerDegLat * cosLat)

	latCellsSpan := int(math.Floor((lat+dLat)/Res5DegLat)) - int(math.Floor((lat-dLat)/Res5DegLat))
	lngCellsSpan := int(math.Floor((lng+dLng)/Res5DegLat)) - int(math.Floor((lng-dLng)/Res5DegLat))

	seen := map[int]struct{}{}
	baseLat := int32(math.Floor((lat - dLat) / Res5DegLat))
	baseLng := int32(math.Floor((lng - dLng) / Res5DegLat))
	for i := 0; i <= latCellsSpan; i++ {
		for j := 0; j <= lngCellsSpan; j++ {
			c := Cell{Lat: baseLat + int32(i), Lng: baseLng + int32(j)}
			seen[CellToShard(c)] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out
}

// CellsForQuery returns the res-5 cells whose squares intersect the query disk
// (a point + radius in metres). The engine scans only these cells' document
// buckets, so query cost tracks docs-near-the-point rather than the whole index.
func CellsForQuery(lat, lng, radiusM float64) []Cell {
	dLat := radiusM / metersPerDegLat
	cosLat := math.Cos(lat * math.Pi / 180)
	if cosLat < 1e-6 {
		cosLat = 1e-6
	}
	dLng := radiusM / (metersPerDegLat * cosLat)

	baseLat := int32(math.Floor((lat - dLat) / Res5DegLat))
	baseLng := int32(math.Floor((lng - dLng) / Res5DegLat))
	topLat := int32(math.Floor((lat + dLat) / Res5DegLat))
	topLng := int32(math.Floor((lng + dLng) / Res5DegLat))

	cells := make([]Cell, 0, int(topLat-baseLat+1)*int(topLng-baseLng+1))
	for la := baseLat; la <= topLat; la++ {
		for ln := baseLng; ln <= topLng; ln++ {
			cells = append(cells, Cell{Lat: la, Lng: ln})
		}
	}
	return cells
}

// haversineM is the great-circle distance in metres between two points. Used to
// rank hits by proximity and to filter to a query radius.
func haversineM(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}
