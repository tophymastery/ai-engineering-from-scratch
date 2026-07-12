package plane

import (
	"sync"
	"time"
)

// geostore.go — the H3 res-7 geo index (D15): a Redis-Cluster stand-in keyed by
// salted res-7 cell with a 30 s TTL, plus the published kNN read contract dispatch
// consumes (find the K nearest drivers to a point). No Redis daemon in this
// sandbox, so — like services/feed-cache/cache/store.go stands in for Redis's
// `SET key val EX ttl` — this is a concurrent map under the injected Clock giving
// the SAME 30 s-TTL geo semantics. What is REAL and load-bearing: the salted-key
// write path (hottest key < 2% of writes), the TTL expiry, and the exact kNN.
//
// DefaultTTL is D15's 30 s live-position TTL.
const DefaultTTL = 30 * time.Second

// Position is a driver's last known position in the geo index.
type Position struct {
	DriverID   string    `json:"driver_id"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	Cell       Cell      `json:"-"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Neighbor is one kNN result: a driver + its geodesic distance to the query point.
type Neighbor struct {
	DriverID   string  `json:"driver_id"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	DistanceM  float64 `json:"distance_m"`
	H3Cell     string  `json:"h3_cell"`
	RecordedAt string  `json:"recorded_at,omitempty"`
}

type geoEntry struct {
	pos      Position
	storedAt time.Time
}

// GeoStore is the salted, TTL'd H3 res-7 geo index.
type GeoStore struct {
	clock Clock
	ttl   time.Duration

	mu      sync.RWMutex
	buckets map[string]map[string]geoEntry // salted cell key -> driver_id -> entry
	where   map[string]string              // driver_id -> current salted cell key
	cells   map[Cell]int                   // unsalted cell -> entry count (kNN ring-stop extent)

	// write histogram (the hottest-key<2% proof reads these; real counters).
	totalWrites int64
	keyWrites   map[string]int64 // salted cell key -> cumulative write count
}

// NewGeoStore builds the geo index over the injected clock with the given TTL
// (pass DefaultTTL for the D15 30 s window).
func NewGeoStore(clock Clock, ttl time.Duration) *GeoStore {
	if clock == nil {
		clock = SystemClock{}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &GeoStore{
		clock:     clock,
		ttl:       ttl,
		buckets:   map[string]map[string]geoEntry{},
		where:     map[string]string{},
		cells:     map[Cell]int{},
		keyWrites: map[string]int64{},
	}
}

// Update writes a driver's latest position into the salted res-7 cell. If the
// driver moved cells the old bucket entry is removed, so exactly one live entry
// exists per driver (a Redis GEOADD overwrite). Returns the cell written.
func (g *GeoStore) Update(driverID string, lat, lng float64, recordedAt time.Time) Cell {
	cell := LatLngToCell(lat, lng)
	key := PhysicalKey(cell, driverID)

	g.mu.Lock()
	defer g.mu.Unlock()

	// Drop the previous bucket entry if the driver moved keys, and keep the
	// per-cell occupancy count accurate (it bounds the kNN ring expansion).
	prev, hadPrev := g.where[driverID]
	if hadPrev && prev != key {
		if b := g.buckets[prev]; b != nil {
			if old, ok := b[driverID]; ok {
				g.decCell(old.pos.Cell)
			}
			delete(b, driverID)
			if len(b) == 0 {
				delete(g.buckets, prev)
			}
		}
	}
	if !hadPrev || prev != key {
		g.cells[cell]++ // new driver, or driver moved into this cell
	}
	b := g.buckets[key]
	if b == nil {
		b = map[string]geoEntry{}
		g.buckets[key] = b
	}
	b[driverID] = geoEntry{
		pos:      Position{DriverID: driverID, Lat: lat, Lng: lng, Cell: cell, RecordedAt: recordedAt},
		storedAt: g.clock.Now(),
	}
	g.where[driverID] = key

	g.totalWrites++
	g.keyWrites[key]++
	return cell
}

// decCell decrements a cell's occupancy count, removing it at zero.
func (g *GeoStore) decCell(c Cell) {
	if g.cells[c] <= 1 {
		delete(g.cells, c)
		return
	}
	g.cells[c]--
}

// live reports whether an entry is still within the TTL window at now.
func (g *GeoStore) live(e geoEntry, now time.Time) bool {
	return now.Sub(e.storedAt) < g.ttl
}

// LiveCount returns the number of drivers with an unexpired position (diagnostics
// + the kNN "seen everyone" termination).
func (g *GeoStore) LiveCount() int {
	now := g.clock.Now()
	g.mu.RLock()
	defer g.mu.RUnlock()
	n := 0
	for _, b := range g.buckets {
		for _, e := range b {
			if g.live(e, now) {
				n++
			}
		}
	}
	return n
}

// eachInCell calls fn for every live position in one res-7 cell across every salt
// sub-key (the scatter-gather a salted read must do). Caller holds the read lock.
// Returns the number of live positions visited.
func (g *GeoStore) eachInCell(c Cell, now time.Time, fn func(Position)) int {
	ck := c.Key()
	n := 0
	for salt := 0; salt < NumSalts; salt++ {
		b := g.buckets[SaltedKey(ck, salt)]
		if b == nil {
			continue
		}
		for _, e := range b {
			if g.live(e, now) {
				fn(e.pos)
				n++
			}
		}
	}
	return n
}

// KNN returns the k drivers nearest to (lat,lng) by geodesic distance, nearest
// first (ties broken by driver_id for determinism). It expands res-7 rings
// outward from the query cell and STOPS as soon as the k-th nearest candidate is
// provably closer than any driver in an unseen ring — so the result is the EXACT
// k-nearest (verified against brute force in knn_test.go), not an approximation.
// This is the read contract dispatch consumes: order's cell + widening rings.
func (g *GeoStore) KNN(lat, lng float64, k int) []Neighbor {
	if k <= 0 {
		return nil
	}
	now := g.clock.Now()
	center := LatLngToCell(lat, lng)

	g.mu.RLock()
	defer g.mu.RUnlock()

	// Total occupied res-7 cells, O(1). Ring expansion stops once we have visited
	// every occupied cell (we've then seen every live driver — the correct stop
	// when live drivers < k or entries have expired), independent of the geodesic
	// early-stop below. Bounds the loop to the DATA EXTENT, never a fixed 4096.
	occupied := len(g.cells)

	// A bounded size-k max-heap keeps only the k nearest seen so far, so a query in
	// a dense cell costs O(candidates · log k) — not an O(C log C) sort of every
	// candidate. heap[0] is the current k-th nearest (the exact-stop bound checks
	// against it in O(1)).
	h := &nbrHeap{lat: lat, lng: lng, k: k}
	coveredCells := 0
	var r int32
	for {
		for _, c := range center.Ring(r) {
			if _, ok := g.cells[c]; ok {
				coveredCells++
			}
			g.eachInCell(c, now, func(p Position) { h.offer(p) })
		}
		// (a) Visited every occupied cell ⇒ we've seen every live driver.
		if coveredCells >= occupied {
			break
		}
		// (b) Exact geodesic stop: after gathering rings 0..r, any driver we have
		// NOT seen sits in a cell at Chebyshev distance >= r+1, whose nearest
		// possible point is >= r*CellMeters from the query cell. So once the heap
		// holds k drivers AND its k-th nearest is within r*CellMeters, no unseen
		// driver can beat it — the top-k is final.
		if h.full() && h.maxDist() <= float64(r)*CellMeters {
			break
		}
		r++
	}
	return h.sorted()
}

// --- write-distribution introspection (the hottest-key<2% proof) ---

// KeyHistogram returns a snapshot of the cumulative write count per physical geo
// key (salted cell) and the total. The salt-balance test measures the hottest
// key's fraction of total writes against the 2% budget on this REAL histogram.
func (g *GeoStore) KeyHistogram() (perKey map[string]int64, total int64) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	perKey = make(map[string]int64, len(g.keyWrites))
	for k, v := range g.keyWrites {
		perKey[k] = v
	}
	return perKey, g.totalWrites
}

// HottestKeyFraction returns the fraction of all writes that landed on the single
// busiest physical geo key (the D15 hot-partition metric).
func (g *GeoStore) HottestKeyFraction() (frac float64, hottestKey string, hottest, total int64) {
	perKey, total := g.KeyHistogram()
	for k, v := range perKey {
		if v > hottest {
			hottest, hottestKey = v, k
		}
	}
	if total > 0 {
		frac = float64(hottest) / float64(total)
	}
	return frac, hottestKey, hottest, total
}
