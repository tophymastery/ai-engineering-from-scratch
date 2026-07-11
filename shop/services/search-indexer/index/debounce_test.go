package index

import (
	"testing"
	"time"
)

// TestRatingDebounce_FloodOnePerWindow is the D17 correctness property: flooding
// a merchant with rating updates yields ≤1 index write per merchant per 5-minute
// window. It uses the injected ManualClock and ADVANCES time in small steps
// (never sleeps), then asserts the exact number of index writes.
func TestRatingDebounce_FloodOnePerWindow(t *testing.T) {
	t0 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	clock := NewManualClock(t0)
	eng := NewEngine(EngineOptions{Clock: clock, IngestWorkers: 2})
	defer eng.Close()

	merchant := "mer_hotspot_000000000000000000"
	// Seed the merchant so rating writes have a document to land on.
	eng.IndexMerchant(MerchantDoc{MerchantID: merchant, Name: "Hotspot", Lat: 13.75, Lng: 100.50, Open: true, MenuVersion: 1})

	// Flood 1000 rating updates spread across ONE 5-minute window (advance 250ms
	// between each: 1000 * 250ms = 250s < 300s window).
	const floods = 1000
	writes := 0
	for i := 0; i < floods; i++ {
		if eng.ApplyRating(merchant, 4.0+float64(i%10)*0.05, int64(i+1), int64(i+1), clock.Now()) {
			writes++
		}
		clock.Advance(250 * time.Millisecond)
	}
	// Exactly one write should have happened inside the first window (the very
	// first offer), everything else debounced.
	if writes != 1 {
		t.Fatalf("window-1 flood: %d index writes, want exactly 1 (debounce ≤1/merchant/5min FAILED)", writes)
	}
	t.Logf("window-1: %d rating updates in -> %d index write(s) out", floods, writes)

	// Cross into the SECOND window and flush: the coalesced latest aggregate is
	// written exactly once more.
	clock.Advance(DebounceWindow) // now well past the first write + window
	flushed := eng.FlushRatings(clock.Now())
	if flushed != 1 {
		t.Fatalf("post-window flush wrote %d, want exactly 1 (the coalesced latest)", flushed)
	}

	// Move past the window opened by the flush write, then a fresh burst gets one
	// immediate write and debounces the rest.
	clock.Advance(DebounceWindow)
	writes2 := 0
	for i := 0; i < 500; i++ {
		if eng.ApplyRating(merchant, 4.9, int64(2000+i), int64(2000+i), clock.Now()) {
			writes2++
		}
		clock.Advance(100 * time.Millisecond)
	}
	if writes2 != 1 {
		t.Fatalf("window-2 flood: %d index writes, want exactly 1", writes2)
	}
	t.Logf("window-2: 500 updates in -> %d immediate write(s); total index writes across ~2 windows kept to the debounce bound", writes2)
}

// TestRatingDebounce_LWWCoalesce checks the coalesced write keeps the freshest
// aggregate by version (LWW), even if a stale event arrives last.
func TestRatingDebounce_LWWCoalesce(t *testing.T) {
	t0 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	clock := NewManualClock(t0)
	d := newRatingDebouncer(clock, DebounceWindow)

	m := "mer_x"
	// First offer writes immediately (v10).
	agg, w := d.Offer(m, ratingAgg{Rating: 4.1, Count: 10, Version: 10})
	if !w || agg.Version != 10 {
		t.Fatalf("first offer: write=%v version=%d, want write=true version=10", w, agg.Version)
	}
	// Within the window: a fresher (v12) then a STALE (v11) update; both debounced,
	// pending must keep v12.
	if _, w := d.Offer(m, ratingAgg{Rating: 4.4, Count: 20, Version: 12}); w {
		t.Fatal("v12 within window should be debounced")
	}
	if _, w := d.Offer(m, ratingAgg{Rating: 4.0, Count: 15, Version: 11}); w {
		t.Fatal("v11 within window should be debounced")
	}
	clock.Advance(DebounceWindow)
	out := d.Flush()
	if out[m].Version != 12 {
		t.Fatalf("coalesced flush version=%d, want 12 (LWW kept the freshest, ignored the stale v11)", out[m].Version)
	}
}
