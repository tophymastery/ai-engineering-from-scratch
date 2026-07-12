package plane

import (
	"fmt"
	"testing"
	"time"
)

// tier_test.go — the D15 tiering criterion: the hot ingest path does NOT write raw
// positions to PG. Prove (a) the Flink 1:10 downsample keeps exactly 1 in 10 raw
// frames for Iceberg, and (b) PG receives ONLY per-trip summaries, so the PG write
// rate stays far under the 500 writes/s-per-cell budget. Real ratios.

// TestDownsampleOneInTen: N raw frames per driver ⇒ ceil(N/10) Iceberg rows.
func TestDownsampleOneInTen(t *testing.T) {
	tr := NewTiering(DownsampleRatio)
	const drivers, frames = 100, 1000
	for d := 0; d < drivers; d++ {
		id := fmt.Sprintf("drv_%03d", d)
		for f := 0; f < frames; f++ {
			tr.Ingest(id, Position{DriverID: id, Lat: 13.75, Lng: 100.53, RecordedAt: time.Unix(int64(f), 0)})
		}
	}
	st := tr.Stats()
	wantIceberg := int64(drivers) * (frames / DownsampleRatio) // 100 * 100
	if st.RawFrames != int64(drivers*frames) {
		t.Fatalf("raw frames: got %d want %d", st.RawFrames, drivers*frames)
	}
	if st.IcebergRows != wantIceberg {
		t.Fatalf("iceberg rows: got %d want %d (1:%d downsample)", st.IcebergRows, wantIceberg, DownsampleRatio)
	}
	if st.PGRows != 0 {
		t.Fatalf("PG rows written on the raw path: got %d want 0 (raw never hits PG)", st.PGRows)
	}
	ratio := float64(st.RawFrames) / float64(st.IcebergRows)
	t.Logf("tiering: raw=%d iceberg=%d (ratio %.1f:1) PG=%d — raw positions never reach PG",
		st.RawFrames, st.IcebergRows, ratio, st.PGRows)
}

// TestPGWriteRateUnderBudget: simulate one cell's ingest for a wall-second and
// assert PG writes/s stays under the 500/s-per-cell budget — because PG gets ONE
// summary per completed trip, not per position.
func TestPGWriteRateUnderBudget(t *testing.T) {
	tr := NewTiering(DownsampleRatio)

	// One busy res-7 cell: 2000 drivers each streaming at 1 Hz for a simulated
	// second (2000 raw frames), of which some finish a trip this second.
	const driversInCell = 2000
	const rawPerDriverThisSecond = 1 // 1 Hz on-job sampling (D14)
	rawWrites := 0
	for d := 0; d < driversInCell; d++ {
		id := fmt.Sprintf("drv_%05d", d)
		for f := 0; f < rawPerDriverThisSecond; f++ {
			tr.Ingest(id, Position{DriverID: id, Lat: 13.746, Lng: 100.534, RecordedAt: time.Unix(int64(f), 0)})
			rawWrites++
		}
	}
	// Trip completions in this cell this second: a delivery every few minutes per
	// driver ⇒ a small fraction complete now. Even a generous 5% completing at once
	// is 100 PG writes/s — under 500. Model 5% completing this second.
	completions := driversInCell * 5 / 100
	for d := 0; d < completions; d++ {
		id := fmt.Sprintf("drv_%05d", d)
		tr.CloseTrip(fmt.Sprintf("trip_%05d", d), id, fmt.Sprintf("ord_%05d", d),
			Position{Lat: 13.746, Lng: 100.534, RecordedAt: time.Unix(0, 0)},
			Position{Lat: 13.748, Lng: 100.536, RecordedAt: time.Unix(1, 0)})
	}

	st := tr.Stats()
	pgPerSec := st.PGRows // one simulated second
	t.Logf("cell ingest 1s: raw=%d, PG writes=%d/s (trip summaries only), budget 500/s",
		rawWrites, pgPerSec)
	if pgPerSec >= 500 {
		t.Fatalf("PG writes = %d/s per cell >= 500/s budget (D15 tiering FAILED)", pgPerSec)
	}
	// And the raw-to-PG ratio must be enormous (raw >> PG).
	if st.PGRows > 0 && st.RawFrames/st.PGRows < 10 {
		t.Fatalf("raw:PG ratio too small: raw=%d pg=%d", st.RawFrames, st.PGRows)
	}
	t.Logf("raw:PG ratio = %d:%d = %dx — PG carries summaries only, never the ultra-hot raw path",
		st.RawFrames, st.PGRows, st.RawFrames/max64(st.PGRows, 1))
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
