package main

import (
	"context"
	"testing"
	"time"
)

// saga_test.go proves EVERY compensation path in the 01 §4 saga:
//   - payment fails            ⇒ PAYMENT_PENDING → CANCELLED (no void; auth failed)
//   - user cancels (pre-pay)   ⇒ PAYMENT_PENDING → CANCELLED [void the hold]
//   - merchant rejects         ⇒ PAID → CANCELLED [refund]
//   - T_accept times out       ⇒ PAID → CANCELLED [refund] (durable timer)
//   - T_dispatch exhausted     ⇒ ACCEPTED → CANCELLED [refund] (durable timer)
//   - driver abandons          ⇒ DISPATCHED → ACCEPTED [re-dispatch] then exhaust
// Payment effects are asserted via the counting client (exact counts).

func toPaid(t *testing.T, srv *server, key string) string {
	t.Helper()
	id := checkout(t, srv, key)
	inject(t, srv, id, "payment.authorized", "evt-auth-"+key)
	assertStatus(t, srv.mux(), id, "PAID")
	return id
}

// TestComp_PaymentFailed: PAYMENT_PENDING --(payment.failed)--> CANCELLED, no void
// (the authorization failed, so there is nothing to reverse).
func TestComp_PaymentFailed(t *testing.T) {
	srv, _, pay := newTestServer(t)
	id := checkout(t, srv, "k")
	inject(t, srv, id, "payment.failed", "evt-fail-1")
	assertStatus(t, srv.mux(), id, "CANCELLED")
	if _, _, void, refund := pay.counts(); void != 0 || refund != 0 {
		t.Fatalf("payment.failed should not void/refund; got void=%d refund=%d", void, refund)
	}
}

// TestComp_MerchantReject: PAID --(merchant reject)--> CANCELLED [refund]. With no
// capture yet, the refund reverses the held auth (a void).
func TestComp_MerchantReject(t *testing.T) {
	srv, _, pay := newTestServer(t)
	id := toPaid(t, srv, "k")
	code, m := do(t, srv.mux(), "POST", "/v1/orders/"+id+":reject", "{}", "")
	if code != 200 || m["status"] != "CANCELLED" {
		t.Fatalf("reject -> %d %v", code, m)
	}
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("merchant reject refund: void count %d want 1", void)
	}
}

// TestComp_AcceptTimeout: PAID with no merchant accept in T_accept ⇒ the durable
// T_accept timer fires TrigAcceptTimeout ⇒ CANCELLED [refund].
func TestComp_AcceptTimeout(t *testing.T) {
	srv, clk, pay := newTestServer(t)
	id := toPaid(t, srv, "k")
	// Nobody accepts. Advance past T_accept and sweep.
	clk.Advance(DefaultAcceptWindow + time.Minute)
	fired, err := srv.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fired != 1 {
		t.Fatalf("accept-timeout sweep fired %d want 1", fired)
	}
	assertStatus(t, srv.mux(), id, "CANCELLED")
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("accept-timeout refund: void count %d want 1", void)
	}
}

// TestComp_DispatchExhausted: ACCEPTED with no driver in T_dispatch ⇒ the durable
// T_dispatch timer fires TrigDispatchExhausted ⇒ CANCELLED [refund].
func TestComp_DispatchExhausted(t *testing.T) {
	srv, clk, pay := newTestServer(t)
	id := toPaid(t, srv, "k")
	if code, _ := do(t, srv.mux(), "POST", "/v1/orders/"+id+":accept", "{}", ""); code != 200 {
		t.Fatalf("accept failed: %d", code)
	}
	assertStatus(t, srv.mux(), id, "ACCEPTED")
	clk.Advance(DefaultDispatchWindow + time.Minute)
	fired, err := srv.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fired != 1 {
		t.Fatalf("dispatch-exhausted sweep fired %d want 1", fired)
	}
	assertStatus(t, srv.mux(), id, "CANCELLED")
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("dispatch-exhausted refund: void count %d want 1", void)
	}
}

// TestComp_DriverAbandon_Redispatch: DISPATCHED --(driver abandon)--> ACCEPTED
// (re-dispatch, no payment effect), and the re-armed T_dispatch timer then
// exhausts ⇒ CANCELLED [refund]. Proves the re-dispatch loop + its terminal
// compensation.
func TestComp_DriverAbandon_Redispatch(t *testing.T) {
	srv, clk, pay := newTestServer(t)
	id := toPaid(t, srv, "k")
	do(t, srv.mux(), "POST", "/v1/orders/"+id+":accept", "{}", "")
	inject(t, srv, id, "dispatch.assigned", "evt-dsp-1")
	assertStatus(t, srv.mux(), id, "DISPATCHED")

	// Driver abandons -> back to ACCEPTED, re-arm T_dispatch, NO payment effect.
	inject(t, srv, id, "driver.abandoned", "evt-aband-1")
	assertStatus(t, srv.mux(), id, "ACCEPTED")
	if _, _, void, refund := pay.counts(); void != 0 || refund != 0 {
		t.Fatalf("re-dispatch should have no payment effect; void=%d refund=%d", void, refund)
	}
	// Re-dispatch also finds no driver -> the re-armed timer exhausts -> refund.
	clk.Advance(DefaultDispatchWindow + time.Minute)
	if _, err := srv.sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	assertStatus(t, srv.mux(), id, "CANCELLED")
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("post-redispatch exhaust refund: void %d want 1", void)
	}
}

// TestComp_UserCancelVoids: PAYMENT_PENDING --(user cancel)--> CANCELLED [void].
func TestComp_UserCancelVoids(t *testing.T) {
	srv, _, pay := newTestServer(t)
	id := checkout(t, srv, "k")
	do(t, srv.mux(), "POST", "/v1/orders/"+id+":cancel", "{}", "")
	assertStatus(t, srv.mux(), id, "CANCELLED")
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("user cancel void %d want 1", void)
	}
}

// TestIllegalTransition_HTTP_409: an action illegal for the current state ⇒ 409
// ORDER_INVALID_TRANSITION through the HTTP surface (accept a PAYMENT_PENDING).
func TestIllegalTransition_HTTP_409(t *testing.T) {
	srv, _, _ := newTestServer(t)
	id := checkout(t, srv, "k") // PAYMENT_PENDING
	code, m := do(t, srv.mux(), "POST", "/v1/orders/"+id+":accept", "{}", "")
	if code != 409 || errCode(m) != "ORDER_INVALID_TRANSITION" {
		t.Fatalf("accept from PAYMENT_PENDING -> %d %s (want 409 ORDER_INVALID_TRANSITION)", code, errCode(m))
	}
}
