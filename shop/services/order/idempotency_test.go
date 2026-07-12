package main

import (
	"context"
	"sync"
	"testing"
)

// idempotency_test.go is the D9 flagship + the D22 inbox proof, combined into the
// V-T9 test criterion:
//
//	"Double 'Pay' tap + BFF retry + Kafka redelivery fixture ⇒ exactly one order
//	 effect."
//
// THREE independent redundancy sources all converge to ONE effect:
//   1. double "Pay" tap   — two concurrent POST /v1/orders, same Idempotency-Key
//   2. BFF retry          — a third POST /v1/orders, same Idempotency-Key
//        ⇒ the D9 idempotency layer: ONE order row, ONE authorization (charge)
//   3. Kafka redelivery   — payment.authorized delivered twice, same event_id
//        ⇒ the durable inbox: ONE PAID transition, ONE order.paid event
// Everything runs under `-race`.

// TestExactlyOneEffect_TripleRedundancy is the headline exactly-once test.
func TestExactlyOneEffect_TripleRedundancy(t *testing.T) {
	srv, _, pay := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()
	const key = "pay-tap-key-ABC" // the SAME Idempotency-Key for tap+tap+retry

	// (1)+(2): two concurrent taps + a retry, all with the SAME key.
	type res struct {
		code int
		id   string
	}
	results := make([]res, 3)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ { // two concurrent "Pay" taps
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, m := do(t, h, "POST", "/v1/orders", checkoutBody, key)
			id, _ := m["order_id"].(string)
			results[i] = res{code, id}
		}(i)
	}
	wg.Wait()
	// (2) BFF retry with the same key (sequential, after the taps settle).
	code, m := do(t, h, "POST", "/v1/orders", checkoutBody, key)
	results[2] = res{code, m["order_id"].(string)}

	// EXACTLY ONE order row.
	n, _ := srv.st.orderCount(ctx)
	if n != 1 {
		t.Fatalf("order count %d want 1 (double-tap + retry created duplicates!)", n)
	}
	// All successful responses reference the SAME order_id.
	var orderID string
	for _, r := range results {
		if r.id != "" {
			if orderID == "" {
				orderID = r.id
			} else if r.id != orderID {
				t.Fatalf("responses disagree on order_id: %q vs %q", orderID, r.id)
			}
		}
	}
	if orderID == "" {
		t.Fatalf("no successful checkout among %+v", results)
	}
	// EXACTLY ONE authorization (charge) — the fresh path ran once.
	if auth, _, _, _ := pay.counts(); auth != 1 {
		t.Fatalf("authorize (charge) count %d want 1 across tap+tap+retry", auth)
	}
	// EXACTLY ONE order.created event.
	if oc, _ := srv.st.outboxCountTopic(ctx, "order.created"); oc != 1 {
		t.Fatalf("order.created outbox %d want 1", oc)
	}

	// (3): Kafka redelivery — the SAME payment.authorized event_id delivered
	// TWICE. The durable inbox dedupes ⇒ exactly one PAID transition.
	const evID = "evt-authorized-XYZ"
	inject(t, srv, orderID, "payment.authorized", evID)
	inject(t, srv, orderID, "payment.authorized", evID) // redelivery, same event_id

	assertStatus(t, h, orderID, "PAID")
	paidCount, _ := srv.st.transitionCount(ctx, orderID, TrigPaymentAuthorized)
	if paidCount != 1 {
		t.Fatalf("PAID transition count %d want 1 (inbox failed to dedupe redelivery)", paidCount)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "order.paid"); oc != 1 {
		t.Fatalf("order.paid outbox %d want 1 (redelivery double-emitted)", oc)
	}
	inboxN, _ := srv.st.inbx.Count(ctx)
	t.Logf("EXACTLY-ONE-EFFECT: orders=1, charges=1, order.created=1, PAID transitions=1, order.paid=1 (inbox rows=%d) under tap+tap+retry+redelivery", inboxN)
}

// TestIdempotency_ReplayHeader: a repeat checkout with the same key replays the
// stored response with Idempotency-Replayed: true (02 §3), and does NOT re-charge.
func TestIdempotency_ReplayHeader(t *testing.T) {
	srv, _, pay := newTestServer(t)
	h := srv.mux()
	code1, m1, _ := doResp(t, h, "POST", "/v1/orders", checkoutBody, "rk")
	if code1 != 201 {
		t.Fatalf("first checkout %d", code1)
	}
	code2, m2, hdr2 := doResp(t, h, "POST", "/v1/orders", checkoutBody, "rk")
	if code2 != 201 {
		t.Fatalf("replay checkout %d", code2)
	}
	if hdr2.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay missing Idempotency-Replayed header")
	}
	if m1["order_id"] != m2["order_id"] {
		t.Fatalf("replay returned different order_id")
	}
	if auth, _, _, _ := pay.counts(); auth != 1 {
		t.Fatalf("replay re-charged: authorize count %d want 1", auth)
	}
}

// TestIdempotency_KeyReuse_409: the same key with a DIFFERENT body ⇒ 409
// IDEMPOTENCY_KEY_REUSED (02 §3).
func TestIdempotency_KeyReuse_409(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.mux()
	if code, _ := do(t, h, "POST", "/v1/orders", `{"quote_id":"qot_a","payment_method_id":"pm"}`, "dup"); code != 201 {
		t.Fatalf("first %d", code)
	}
	code, m := do(t, h, "POST", "/v1/orders", `{"quote_id":"qot_DIFFERENT","payment_method_id":"pm"}`, "dup")
	if code != 409 || errCode(m) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse -> %d %s (want 409 IDEMPOTENCY_KEY_REUSED)", code, errCode(m))
	}
}
