package match

import (
	"fmt"
	"math"
)

// geo.go — the H3-zone geometry (D13) + the deterministic map-sim ETA twin.
//
// D13 assigns drivers per H3 zone, each zone owned by a single writer (Kafka
// partition per zone). This sandbox has no vendored H3 library (the repo's
// std-lib-only ethos, see libs/sharding + services/search-indexer/index/geo.go),
// so — exactly as V-T4 does for search shard routing — we model an H3 res-5 zone
// with a FAITHFUL deterministic equal-angle bin at res-5 scale. In the platform's
// SE-Asia cells (near the equator) a degree of longitude ≈ a degree of latitude,
// so an equal-angle bin is a good geometric stand-in for a res-5 hex. The PROPERTY
// that matters for dispatch — every pickup/driver maps to exactly one zone, and a
// zone is owned by exactly one writer per tick — is exact in this model.

// ZoneDegLat is the side of a dispatch zone in degrees (~14.6 km at the equator,
// matching an H3 res-5 hexagon's ~15 km cell diameter). Identical scale to the
// search-indexer's Res5DegLat so dispatch zones and search shards align.
const ZoneDegLat = 0.1315

// Zone is an H3-res-5-equivalent dispatch zone: the integer grid coordinates of a
// res-5 bin. Deterministic and pure — the same lat/lng always yields the same
// zone. Zone ownership (single-writer-per-zone) keys on this value.
type Zone struct {
	Lat int32 `json:"lat"`
	Lng int32 `json:"lng"`
}

// Point is a WGS84 lat/lng. Merchant pickup points and driver locations are
// Points; an order's pickup Point determines its zone.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// ZoneFor maps a point to its dispatch zone. THE zone-routing primitive: the
// single writer that owns an order (or a driver) is the one that owns this zone.
func ZoneFor(p Point) Zone {
	return Zone{
		Lat: int32(math.Floor(p.Lat / ZoneDegLat)),
		Lng: int32(math.Floor(p.Lng / ZoneDegLat)),
	}
}

// Key is the stable string identity of a zone — the Kafka partition key (D13
// "Kafka partition per zone") and the single-writer lock key. One zone's orders +
// drivers always hash to one partition, so one consumer owns the zone.
func (z Zone) Key() string { return fmt.Sprintf("z_%d_%d", z.Lat, z.Lng) }

// Partition maps a zone to one of n Kafka partitions. Deterministic FNV-1a over
// the zone key so a zone is pinned to a single partition ⇒ a single consumer ⇒ a
// single writer per zone (the D13 invariant). The topology (partition count) is
// render-only in-sandbox; the pinning is real (see partition_test.go).
func (z Zone) Partition(n int) int {
	if n <= 0 {
		return 0
	}
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, b := range []byte(z.Key()) {
		h ^= uint64(b)
		h *= prime
	}
	return int(h % uint64(n))
}

// --- deterministic map-sim ETA twin (S-T7) ---
//
// The dispatch matcher needs a pickup ETA per (driver, order) pair. In production
// it calls the map-sim fake over HTTP (services/fakes/map-sim). For a byte-
// identical deterministic REPLAY (correctness property #1) the matcher takes an
// injected ETAFunc; the default is this local twin, which reproduces map-sim's
// exact formula (haversine × 1.3 road factor ÷ a fixed per-mode speed). Using the
// same formula in-process means a logged snapshot replays to identical ETAs and
// identical assignments without a network hop — see VERIFICATION §V-T12.

// roadFactor inflates straight-line distance to a road path (map-sim's constant).
const roadFactor = 1.3

// driverSpeedMPS is the fixed ground speed of a dispatch driver (metres/second),
// matching map-sim's CAR speed (~30 km/h city).
const driverSpeedMPS = 8.333

// ETASeconds is the deterministic pickup ETA between two points in seconds:
// haversine × road factor ÷ fixed speed — the exact map-sim CAR formula. Pure and
// side-effect-free, so it is a valid ETAFunc for the deterministic matcher.
func ETASeconds(from, to Point) int {
	d := haversineM(from, to) * roadFactor
	return int(math.Round(d / driverSpeedMPS))
}

// haversineM returns the great-circle distance between two points in metres.
func haversineM(a, b Point) float64 {
	const earthR = 6371000.0 // metres
	rad := math.Pi / 180
	la1, la2 := a.Lat*rad, b.Lat*rad
	dLa := (b.Lat - a.Lat) * rad
	dLo := (b.Lng - a.Lng) * rad
	h := math.Sin(dLa/2)*math.Sin(dLa/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLo/2)*math.Sin(dLo/2)
	return 2 * earthR * math.Asin(math.Min(1, math.Sqrt(h)))
}
