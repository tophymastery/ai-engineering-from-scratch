package main

import (
	"context"
	"net/http"
	"testing"
)

// snapshot_test.go — proves the Redis-snapshot + PG durability path (01 §1
// "Redis snapshot + PG"): a read is served from the snapshot tier when fresh, and
// on a snapshot miss (eviction / restart / TTL expiry) the cart is REHYDRATED
// from PostgreSQL and the snapshot repopulated. The assembled view is identical
// across the snapshot and the rehydrate.

// TestSnapshotServesThenRehydrates: after the first read populates the snapshot,
// a repeat read is a snapshot HIT; evicting the snapshot (simulated Redis flush)
// forces a REHYDRATE from PG that returns the identical cart.
func TestSnapshotServesThenRehydrates(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)
	do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 2))

	// First GET rehydrates from PG (the add already primed the snapshot, so this
	// is a hit) — read once to establish the baseline view.
	code, etag, before := do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "")
	if code != 200 {
		t.Fatalf("first get: %d", code)
	}
	hitsBefore := s.snap.stats()["hits"]

	// A repeat read must be a snapshot HIT (no rehydrate).
	rehydrBefore := s.snap.stats()["rehydrates"]
	do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "")
	if got := s.snap.stats()["hits"]; got <= hitsBefore {
		t.Fatalf("repeat read was not a snapshot hit (hits %d → %d)", hitsBefore, got)
	}
	if got := s.snap.stats()["rehydrates"]; got != rehydrBefore {
		t.Fatalf("repeat read should not rehydrate (rehydrates %d → %d)", rehydrBefore, got)
	}

	// Simulate a Redis eviction / cart-service restart: drop the snapshot.
	s.snap.invalidate(tCart)

	// Next read misses the snapshot → REHYDRATES from PG, returns the identical cart.
	code, etag2, after := do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "")
	if code != 200 {
		t.Fatalf("post-eviction get: %d", code)
	}
	if got := s.snap.stats()["rehydrates"]; got != rehydrBefore+1 {
		t.Fatalf("post-eviction read should rehydrate exactly once (rehydrates %d → %d)", rehydrBefore, got)
	}
	if etag2 != etag {
		t.Fatalf("rehydrated ETag %q != snapshot ETag %q", etag2, etag)
	}
	if subtotal(after) != subtotal(before) {
		t.Fatalf("rehydrated subtotal %d != snapshot subtotal %d", subtotal(after), subtotal(before))
	}
	if len(after["items"].([]any)) != len(before["items"].([]any)) {
		t.Fatal("rehydrated line count differs from the snapshot")
	}
}

// TestRehydrateReflectsRepricedPG: after a menu.updated reprices the PG cart and
// invalidates the snapshot, the rehydrated view reflects the new price — i.e. the
// snapshot never masks the durable store.
func TestRehydrateReflectsRepricedPG(t *testing.T) {
	s, clk, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)
	do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "") // prime snapshot at 8000

	// menu.updated → catalogView + PG repriced to 9000 + snapshot invalidated.
	newAndApply(t, s, "evt_snap_reval", tMerchant, tItem, 9000, 2, clk.Now())

	// The next read rehydrates from PG (snapshot was invalidated) → 9000.
	_, _, m := do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "")
	if got := subtotal(m); got != 9000 {
		t.Fatalf("rehydrated subtotal = %d, want 9000 (repriced PG, not the stale snapshot)", got)
	}
}

// TestPGIsSystemOfRecord: with the snapshot fully cleared, EVERY line is
// reconstructable from PG alone (durable store parity).
func TestPGIsSystemOfRecord(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)
	fc.seed(tMerchant, 1, map[string]itemInfo{
		tItem:      {Name: "Som Tam", Amount: 8000, Currency: "THB", Available: true, Version: 1},
		"itm_larb": {Name: "Larb", Amount: 9000, Currency: "THB", Available: true, Version: 1},
	})
	_, etag, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 2))
	do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag, addBody("itm_larb", tMerchant, 1))

	// Clear the whole snapshot tier (cold Redis).
	s.snap.invalidate(tCart)

	// loadCartView reads PG directly — the authoritative reconstruction.
	v, err := s.st.loadCartView(context.Background(), tCart)
	if err != nil {
		t.Fatalf("load from PG: %v", err)
	}
	if len(v.Items) != 2 {
		t.Fatalf("PG has %d lines, want 2", len(v.Items))
	}
	if v.Subtotal.Amount != 25000 { // 2×8000 + 1×9000
		t.Fatalf("PG subtotal = %d, want 25000", v.Subtotal.Amount)
	}
}
