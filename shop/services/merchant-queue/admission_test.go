package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flashSale fires nOrders order.paid events at one merchant (the "checkout" path)
// then attempts to accept ALL of them CONCURRENTLY inside a frozen admission
// window. It returns (checkout5xx, accept5xx, accepted, deferred, busyBadges).
func flashSale(t *testing.T, srv *server, mid string, nOrders int) (checkout5xx, accept5xx, accepted, deferred, busy int32) {
	t.Helper()
	ctx := context.Background()
	h := srv.mux()

	// 1. CHECKOUT path: every paid order enters the queue. A flash sale must never
	// 5xx here — the projection cannot reject an order.
	ids := make([]string, nOrders)
	for i := 0; i < nOrders; i++ {
		oid := fmt.Sprintf("ord_%s_%05d", mid, i)
		ids[i] = oid
		env, err := makeOrderEnvelope("evt_p_"+oid, TopicOrderPaid, oid, mid, "bkk",
			map[string]any{"paid_at": testBase.Format(time.RFC3339)}, testBase)
		if err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if _, err := srv.pr.InjectEnvelope(ctx, env); err != nil {
			atomic.AddInt32(&checkout5xx, 1) // any projection error is a checkout failure
		}
	}

	// 2. ACCEPT path: attempt to accept all nOrders concurrently in the frozen
	// window. Admission caps grants at capacity; the rest are DEFERRED (busy badge,
	// HTTP 200 — never 5xx).
	var wg sync.WaitGroup
	for _, oid := range ids {
		wg.Add(1)
		go func(oid string) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/merchant/orders/"+oid+":accept", nil)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code >= 500 {
				atomic.AddInt32(&accept5xx, 1)
				return
			}
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			switch out["status"] {
			case "ACCEPTED":
				atomic.AddInt32(&accepted, 1)
			case "PENDING":
				atomic.AddInt32(&deferred, 1)
			}
			if b, _ := out["busy"].(bool); b {
				atomic.AddInt32(&busy, 1)
			}
		}(oid)
	}
	wg.Wait()
	return
}

// TestFlashSale50x: 50× the configured kitchen capacity of orders hit one
// merchant. The DoD: zero checkout 5xx, zero accept 5xx, and accept rate =
// configured capacity ± 5%. Runs under -race (concurrent accepts stress the
// atomic token bucket).
func TestFlashSale50x(t *testing.T) {
	srv, _, foc := newTestServer(t)
	const capacity = DefaultCapacity // 30 / 10 min
	const nOrders = 50 * capacity    // 50× flash sale = 1500 orders on one merchant
	mid := "mer_flash"

	c5, a5, accepted, deferred, busy := flashSale(t, srv, mid, nOrders)

	if c5 != 0 {
		t.Fatalf("checkout 5xx = %d, want 0 (flash sale must never fail checkout)", c5)
	}
	if a5 != 0 {
		t.Fatalf("accept 5xx = %d, want 0", a5)
	}
	if int(accepted+deferred) != nOrders {
		t.Fatalf("accepted(%d)+deferred(%d) = %d, want %d (every accept got a non-5xx answer)", accepted, deferred, accepted+deferred, nOrders)
	}
	// accept rate = configured capacity ± 5%.
	tol := int(math.Ceil(0.05 * float64(capacity)))
	if diff := int(math.Abs(float64(int(accepted) - capacity))); diff > tol {
		t.Fatalf("accepted = %d, want %d ± %d (accept rate off configured capacity)", accepted, capacity, tol)
	}
	if int(foc.acceptCount()) != int(accepted) {
		t.Fatalf("saga accept called %d times but %d orders accepted (must match)", foc.acceptCount(), accepted)
	}
	// Every over-capacity accept is DEFERRED with a busy badge (not failed); once
	// the kitchen is saturated the accepted ones may also carry the busy badge, so
	// busy ≥ deferred and every deferred order is busy.
	if int(deferred) != nOrders-capacity {
		t.Fatalf("deferred = %d, want %d (nOrders - capacity)", deferred, nOrders-capacity)
	}
	if int(busy) < int(deferred) {
		t.Fatalf("busy badges (%d) < deferred accepts (%d) — some deferrals had no busy badge", busy, deferred)
	}
	t.Logf("FLASH SALE 50×: %d orders on one merchant ⇒ checkout 5xx=%d accept 5xx=%d; accepted=%d (capacity %d ±%d), deferred+busy=%d",
		nOrders, c5, a5, accepted, capacity, tol, deferred)

	// The busy badge + inflated ETA is observable on the capacity endpoint.
	pending, _ := srv.st.pendingCount(context.Background(), mid)
	st := srv.adm.Status(mid, testBase, pending)
	if !st.Busy {
		t.Fatalf("capacity endpoint not busy after saturating the kitchen")
	}
	if st.PrepETAMinutes <= baseETAMinutes {
		t.Fatalf("prep ETA %d not inflated above base %d under load", st.PrepETAMinutes, baseETAMinutes)
	}
	t.Logf("BUSY BADGE: merchant busy=%v, prep_eta=%d min (base %d, inflated by backlog=%d)", st.Busy, st.PrepETAMinutes, baseETAMinutes, pending)
}

// TestMerchantTunableCapacity: a merchant raises its capacity to 50 → the accept
// rate follows the new configured capacity ± 5%.
func TestMerchantTunableCapacity(t *testing.T) {
	srv, _, _ := newTestServer(t)
	mid := "mer_tunable"
	const newCap = 50
	srv.adm.SetCapacity(mid, newCap, DefaultWindow)

	_, a5, accepted, _, _ := flashSale(t, srv, mid, 50*newCap)
	if a5 != 0 {
		t.Fatalf("accept 5xx = %d, want 0", a5)
	}
	tol := int(math.Ceil(0.05 * float64(newCap)))
	if diff := int(math.Abs(float64(int(accepted) - newCap))); diff > tol {
		t.Fatalf("accepted = %d, want tuned capacity %d ± %d", accepted, newCap, tol)
	}
	t.Logf("MERCHANT-TUNABLE: capacity tuned to %d ⇒ accepted=%d (±%d)", newCap, accepted, tol)
}

// TestAdmissionWindowSliding: after the window elapses, capacity refreshes.
func TestAdmissionWindowSliding(t *testing.T) {
	adm := newAdmission()
	mid := "mer_win"
	now := testBase
	granted := 0
	for i := 0; i < DefaultCapacity*2; i++ {
		if adm.TryAccept(mid, now) {
			granted++
		}
	}
	if granted != DefaultCapacity {
		t.Fatalf("granted %d in window, want %d", granted, DefaultCapacity)
	}
	// Advance past the window → tokens refresh.
	later := now.Add(DefaultWindow + time.Second)
	granted2 := 0
	for i := 0; i < DefaultCapacity; i++ {
		if adm.TryAccept(mid, later) {
			granted2++
		}
	}
	if granted2 != DefaultCapacity {
		t.Fatalf("after window, granted %d, want %d (sliding window did not refresh)", granted2, DefaultCapacity)
	}
}
