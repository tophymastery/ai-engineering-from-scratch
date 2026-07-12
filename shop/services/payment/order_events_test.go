package main

import (
	"context"
	"testing"
)

// order_events_test.go proves the payment slice CONSUMES order contract stubs
// (the event-level order↔payment integration) with exactly-once effect:
//   - order.delivered ⇒ auto-capture the held authorization (checkout→authorize,
//     delivered→capture — the task's upstream drive).
//   - order.cancelled ⇒ void the uncaptured hold.
// A redelivered order event (same event_id) is a no-op (durable inbox dedupe).

// injectOrder delivers an order.* envelope through the consumer with an explicit
// event_id (so a test can redeliver the SAME one).
func injectOrder(t *testing.T, srv *server, orderID, eventType, eventID string) {
	t.Helper()
	env, err := makeOrderEnvelope(eventID, eventType, orderID, "bkk", map[string]any{"order_id": orderID}, srv.clock.Now())
	if err != nil {
		t.Fatalf("makeOrderEnvelope: %v", err)
	}
	if _, err := srv.orders.InjectEnvelope(context.Background(), env); err != nil {
		t.Fatalf("inject %s: %v", eventType, err)
	}
}

// TestOrderDelivered_AutoCaptures: order.delivered ⇒ the payment is captured
// exactly once; redelivery of the same event_id is a no-op.
func TestOrderDelivered_AutoCaptures(t *testing.T) {
	srv, _, psp := newTestServer(t)
	ctx := context.Background()
	id := authorize(t, srv, "a", "ord_deliv", goodCard)

	injectOrder(t, srv, "ord_deliv", "order.delivered", "evt_deliv_1")
	p, _, _ := srv.st.getPayment(ctx, id)
	if p.Status != StateCaptured {
		t.Fatalf("after order.delivered status %s want CAPTURED", p.Status)
	}
	if _, c, _ := psp.counts(); c != 1 {
		t.Fatalf("PSP capture count %d want 1", c)
	}
	// Redelivery of the SAME event_id ⇒ still one capture (inbox dedupe).
	injectOrder(t, srv, "ord_deliv", "order.delivered", "evt_deliv_1")
	if _, c, _ := psp.counts(); c != 1 {
		t.Fatalf("redelivered order.delivered double-captured: %d want 1", c)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.captured"); oc != 1 {
		t.Fatalf("payment.captured %d want 1", oc)
	}
}

// TestOrderCancelled_Voids: order.cancelled ⇒ the uncaptured hold is voided
// exactly once.
func TestOrderCancelled_Voids(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	id := authorize(t, srv, "a", "ord_cxl", goodCard)

	injectOrder(t, srv, "ord_cxl", "order.cancelled", "evt_cxl_1")
	p, _, _ := srv.st.getPayment(ctx, id)
	if p.Status != StateVoided {
		t.Fatalf("after order.cancelled status %s want VOIDED", p.Status)
	}
	injectOrder(t, srv, "ord_cxl", "order.cancelled", "evt_cxl_1") // redelivery
	if n, _ := srv.st.transitionCount(ctx, id, "void:order.cancelled"); n != 1 {
		t.Fatalf("void transitions %d want 1 under redelivery", n)
	}
}

// TestOrderEvent_UnknownOrder_NoOp: an order event for an order with no payment
// commits its inbox dedupe row and does nothing (no crash, no effect).
func TestOrderEvent_UnknownOrder_NoOp(t *testing.T) {
	srv, _, psp := newTestServer(t)
	injectOrder(t, srv, "ord_ghost", "order.delivered", "evt_ghost")
	if _, c, _ := psp.counts(); c != 0 {
		t.Fatalf("captured a nonexistent payment: %d", c)
	}
}
