package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// failover_test.go proves the D9 HEADLINE for the money-mutation flagship:
//
//	"Forced Redis failover during a 1.5× checkout storm ⇒ zero duplicate charges,
//	 zero lost orders."
//
// Redis is DEMOTED to an advisory read-through cache (libs/idempotency); the PG
// UNIQUE(idempotency_key) insert inside the money-mutation transaction is the
// source of truth. So failing the cache MID-STORM must NOT double-charge or lose
// a payment. We simulate the "forced Redis failover" by DROPPING the in-process
// SwappableCache partway through a concurrent authorize storm (retries +
// double-taps); after the drop every mutation falls through to the durable PG
// path. At the end we assert, by real row/charge counts under `-race`:
//   - exactly one charge per order (zero duplicates)
//   - every intended order has its charge (zero lost)
//
// Sandbox adaptation (disclosed in VERIFICATION §V-T10): "forced Redis failover"
// = SwappableCache.Drop() (the demoted cache flips to miss/no-op mid-storm, as a
// Redis FLUSHALL/failover would); the PG-UNIQUE-is-truth property is FULL, the
// literal 1.5× peak throughput is the V-T31 load-harness seam (this runs a
// bounded concurrent storm — the invariant, not the scale, is the point).

// TestFailover_Storm_ZeroDuplicateZeroLost is the money-invariant chaos test.
func TestFailover_Storm_ZeroDuplicateZeroLost(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()

	const orders = 300     // distinct orders in the storm
	const perOrder = 3     // concurrent submissions per order (2 double-taps + 1 retry), SAME key
	dropAt := int32(orders * perOrder / 2)

	var submitted int32
	var dropped int32
	succeeded := make([]int32, orders) // per-order: did at least one request get a 201?

	var wg sync.WaitGroup
	for i := 0; i < orders; i++ {
		orderID := fmt.Sprintf("ord_storm_%04d", i)
		key := "authz:" + orderID // one idempotency key per order — all retries dedupe on it
		body := authBody(orderID, goodCard)
		for j := 0; j < perOrder; j++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				// FORCED REDIS FAILOVER: once half the storm has been submitted, drop
				// the advisory cache. Exactly one goroutine performs the drop.
				if atomic.AddInt32(&submitted, 1) >= dropAt && atomic.CompareAndSwapInt32(&dropped, 0, 1) {
					srv.st.cache.Drop()
				}
				code, m := do(t, h, "POST", "/v1/payments:authorize", body, key)
				if code == 201 {
					if _, ok := m["payment_id"].(string); ok {
						atomic.StoreInt32(&succeeded[idx], 1)
					}
				}
			}(i)
		}
	}
	wg.Wait()

	// The failover actually happened mid-storm.
	if !srv.st.cache.Dropped() {
		t.Fatalf("cache was never dropped — the failover was not exercised")
	}

	// ZERO DUPLICATE CHARGES: exactly one charge (PSP authorize) and one payment
	// row per order.
	charges, _, _ := psp.counts()
	if charges != orders {
		t.Fatalf("PSP charges %d want %d (duplicate or lost charge under failover!)", charges, orders)
	}
	if n, _ := srv.st.paymentCount(ctx); n != orders {
		t.Fatalf("payment rows %d want %d", n, orders)
	}
	if n, _ := srv.st.distinctOrderCount(ctx); n != orders {
		t.Fatalf("distinct charged orders %d want %d (an order was charged twice)", n, orders)
	}
	if n, _ := srv.st.paymentCountByStatus(ctx, StateAuthorized); n != orders {
		t.Fatalf("AUTHORIZED payments %d want %d", n, orders)
	}
	// ZERO LOST ORDERS: every intended order got its charge acknowledged.
	lost := 0
	for i := 0; i < orders; i++ {
		if atomic.LoadInt32(&succeeded[i]) == 0 {
			lost++
		}
	}
	if lost != 0 {
		t.Fatalf("%d orders lost their charge (no 201 with a payment_id)", lost)
	}
	// Exactly one payment.authorized event per order (outbox exactly-once too).
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.authorized"); oc != orders {
		t.Fatalf("payment.authorized events %d want %d", oc, orders)
	}
	t.Logf("FAILOVER-STORM: %d orders × %d concurrent submissions, cache DROPPED mid-storm ⇒ charges=%d rows=%d distinct=%d lost=0 (zero duplicate, zero lost)",
		orders, perOrder, charges, orders, orders)
}
