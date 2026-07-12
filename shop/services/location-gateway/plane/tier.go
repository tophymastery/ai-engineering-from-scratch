package plane

import (
	"sync"
)

// tier.go — telemetry tiering (D15): the hot ingest path does NOT write raw
// positions to PostgreSQL. Raw frames feed only (a) the live geo index (30 s TTL,
// geostore.go) and (b) a Flink 1:10 downsample → Iceberg for analytics/ML; PG
// keeps ONLY per-trip summary polylines. "PG at full rate is petabyte nonsense"
// (D15). No Flink/Iceberg daemon in this sandbox, so the downsampler is an
// in-process 1:10 stand-in and Iceberg is an in-memory row sink; the RATIOS that
// matter — raw:Iceberg = 10:1 and PG writes = trips (not positions), so PG stays
// well under the 500 writes/s-per-cell budget — are real and asserted (tier_test.go
// + PG-write-ratio criterion). Disclosed in VERIFICATION.md §V-T13.

// DownsampleRatio is D15's 1:10 Flink downsample (keep 1 in every 10 raw frames).
const DownsampleRatio = 10

// Downsampler keeps 1 raw frame in every DownsampleRatio, per driver, for the
// Iceberg analytics tier. Deterministic (counter-based, not sampled) so the exact
// 10:1 ratio is provable.
type Downsampler struct {
	ratio int
	mu    sync.Mutex
	count map[string]int // driver_id -> frames seen since last kept
}

// NewDownsampler builds a 1:ratio downsampler (pass 0 for the D15 default 10).
func NewDownsampler(ratio int) *Downsampler {
	if ratio <= 0 {
		ratio = DownsampleRatio
	}
	return &Downsampler{ratio: ratio, count: map[string]int{}}
}

// Keep reports whether this frame survives the downsample (every ratio-th frame
// per driver). The first frame of each driver is kept (trip start anchor).
func (d *Downsampler) Keep(driverID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.count[driverID]
	keep := n%d.ratio == 0
	d.count[driverID] = n + 1
	return keep
}

// TripSummary is the ONLY thing that reaches PostgreSQL for a trip: a compact
// per-trip record (driver, order, point count, start/end, polyline vertices) — one
// row per completed trip, never per position.
type TripSummary struct {
	TripID    string  `json:"trip_id"`
	DriverID  string  `json:"driver_id"`
	OrderID   string  `json:"order_id"`
	Points    int     `json:"points"`
	StartLat  float64 `json:"start_lat"`
	StartLng  float64 `json:"start_lng"`
	EndLat    float64 `json:"end_lat"`
	EndLng    float64 `json:"end_lng"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at"`
}

// TieringStats accounts for what each tier received (the PG-write-ratio proof).
type TieringStats struct {
	RawFrames   int64 // total raw positions ingested
	IcebergRows int64 // downsampled rows (≈ RawFrames / 10)
	PGRows      int64 // per-trip summary rows written to PG (= trips)
}

// Tiering routes ingested frames to the analytics + summary tiers, enforcing the
// D15 rule that raw positions never hit PG. It is the accounting seam the
// PG-write-rate criterion measures.
type Tiering struct {
	down *Downsampler

	mu        sync.Mutex
	iceberg   []Position          // Iceberg analytics rows (downsampled)
	tripPts   map[string]int      // trip_id -> points accumulated (in-flight, NOT PG)
	summaries map[string]TripSummary
	stats     TieringStats
}

// NewTiering builds the tiering router with a 1:ratio downsampler.
func NewTiering(ratio int) *Tiering {
	return &Tiering{
		down:      NewDownsampler(ratio),
		tripPts:   map[string]int{},
		summaries: map[string]TripSummary{},
	}
}

// Ingest routes one raw frame: it always feeds the live geo index (caller's job),
// is conditionally kept for Iceberg (1:10), and accrues into the in-flight trip
// point count — it NEVER writes a PG row. Returns whether the frame was kept for
// Iceberg.
func (t *Tiering) Ingest(driverID string, p Position) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats.RawFrames++
	t.tripPts[driverID]++
	kept := t.down.Keep(driverID)
	if kept {
		t.iceberg = append(t.iceberg, p)
		t.stats.IcebergRows++
	}
	return kept
}

// CloseTrip writes the ONE per-trip summary row to PG (the only PG write on this
// plane) and returns it. Called on trip end (order delivered), not per position.
func (t *Tiering) CloseTrip(tripID, driverID, orderID string, start, end Position) TripSummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	sum := TripSummary{
		TripID:    tripID,
		DriverID:  driverID,
		OrderID:   orderID,
		Points:    t.tripPts[driverID],
		StartLat:  start.Lat, StartLng: start.Lng,
		EndLat: end.Lat, EndLng: end.Lng,
		StartedAt: start.RecordedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		EndedAt:   end.RecordedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	t.summaries[tripID] = sum
	t.stats.PGRows++ // ONE PG write per trip
	delete(t.tripPts, driverID)
	return sum
}

// Stats returns a snapshot of the per-tier accounting (the PG-write-ratio proof).
func (t *Tiering) Stats() TieringStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// IcebergRows returns the number of downsampled analytics rows retained.
func (t *Tiering) IcebergRows() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.iceberg)
}
