package match

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var resBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// TestReserveExclusive: a driver can be held by only one order at a time; a second
// Reserve while the first is live is a COUNTED conflict, not a grant — and never a
// 409 (Reserve just returns false).
func TestReserveExclusive(t *testing.T) {
	l := NewLedger(DefaultReservationTTL)
	z := Zone{Lat: 1, Lng: 2}
	if !l.Reserve("drv_1", "ord_a", z, resBase) {
		t.Fatal("first reserve should be granted")
	}
	if l.Reserve("drv_1", "ord_b", z, resBase) {
		t.Fatal("second reserve of a held driver must be refused (exclusive)")
	}
	st := l.Stats(resBase)
	if st.Conflict != 1 || st.Created != 1 {
		t.Fatalf("want created=1 conflict=1, got %+v", st)
	}
}

// TestReservationExpiresAndReoffers: after the 10 s TTL, the hold is reclaimable —
// a fresh Reserve succeeds (time ADVANCED, never slept).
func TestReservationExpiresAndReoffers(t *testing.T) {
	l := NewLedger(10 * time.Second)
	z := Zone{}
	l.Reserve("drv_1", "ord_a", z, resBase)
	// still held at +9s
	if l.Reserve("drv_1", "ord_b", z, resBase.Add(9*time.Second)) {
		t.Fatal("still held at +9s must refuse")
	}
	// expired at +10s ⇒ reclaimable
	if !l.Reserve("drv_1", "ord_b", z, resBase.Add(10*time.Second)) {
		t.Fatal("expired hold must be reclaimable at +10s")
	}
	st := l.Stats(resBase.Add(10 * time.Second))
	// one consumed=0, one released (the lazily-expired first), one live (the new).
	if st.Released != 1 || st.HeldLive != 1 || st.Leaked != 0 {
		t.Fatalf("want released=1 held_live=1 leaked=0, got %+v", st)
	}
}

// TestConsumeAccept: Consume (the accept) only succeeds for the exact live hold;
// after it, the hold is accounted as consumed (not leaked).
func TestConsumeAccept(t *testing.T) {
	l := NewLedger(10 * time.Second)
	l.Reserve("drv_1", "ord_a", Zone{}, resBase)
	if l.Consume("drv_1", "ord_WRONG", resBase) {
		t.Fatal("consume with the wrong order must fail")
	}
	if !l.Consume("drv_1", "ord_a", resBase.Add(3*time.Second)) {
		t.Fatal("consume of the live hold must succeed")
	}
	st := l.Stats(resBase.Add(3 * time.Second))
	if st.Consumed != 1 || st.HeldLive != 0 || st.Leaked != 0 {
		t.Fatalf("want consumed=1 held_live=0 leaked=0, got %+v", st)
	}
}

// TestReservationLeakZeroUnderConcurrency is correctness property #3 (leak rate 0)
// at speed: many goroutines reserve/consume/expire disjoint drivers concurrently
// (under -race); after a final sweep the ledger's exact accounting shows ZERO
// leaked reservations — every hold was either consumed or released.
func TestReservationLeakZeroUnderConcurrency(t *testing.T) {
	l := NewLedger(10 * time.Second)
	const workers, per = 16, 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				drv := fmt.Sprintf("drv_%d_%d", w, i) // disjoint per (worker,i)
				now := resBase.Add(time.Duration(i) * time.Millisecond)
				if !l.Reserve(drv, "ord_"+drv, Zone{Lat: int32(w)}, now) {
					continue
				}
				if i%2 == 0 {
					l.Consume(drv, "ord_"+drv, now) // half accept
				}
				// the other half are left to expire → swept below
			}
		}(w)
	}
	wg.Wait()
	// advance well past any TTL and sweep — reclaims every un-consumed hold.
	l.Sweep(resBase.Add(24 * time.Hour))
	st := l.Stats(resBase.Add(24 * time.Hour))
	t.Logf("reservation ledger after concurrency: %+v", st)
	if st.Leaked != 0 {
		t.Fatalf("reservation leak rate must be 0, leaked=%d (%+v)", st.Leaked, st)
	}
	if st.Created != st.Consumed+st.Released {
		t.Fatalf("accounting broken: created(%d) != consumed(%d)+released(%d)", st.Created, st.Consumed, st.Released)
	}
}
