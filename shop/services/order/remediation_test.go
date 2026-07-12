package main

import (
	"context"
	"testing"
	"time"
)

// remediation_test.go proves the V-T9 test criterion:
//
//	"Remediation fixture auto-voids in < 16 min, exactly once."
//
// A checkout that never advances (stuck PAYMENT_PENDING — e.g. the payment.*
// event never arrives) is auto-remediated by the DURABLE remediation timer armed
// at checkout+15min: when it fires it voids the held authorization and cancels
// the order (PAYMENT_PENDING → CANCELLED [void]). Frozen clock; we advance it,
// never sleep.

// TestRemediation_AutoVoid_Under16Min is the headline remediation test.
func TestRemediation_AutoVoid_Under16Min(t *testing.T) {
	srv, clk, pay := newTestServer(t)
	ctx := context.Background()
	id := checkout(t, srv, "stuck-key") // PAYMENT_PENDING, auth held, remediation armed
	if auth, _, _, _ := pay.counts(); auth != 1 {
		t.Fatalf("checkout authorize %d want 1 (need a held auth to void)", auth)
	}

	// Not yet due at +14 min: nothing fires, order still PAYMENT_PENDING.
	clk.Advance(14 * time.Minute)
	if n, _ := srv.sweeper.SweepOnce(ctx); n != 0 {
		t.Fatalf("remediation fired early (+14m): %d", n)
	}
	assertStatus(t, srv.mux(), id, "PAYMENT_PENDING")

	// Cross the 15-min horizon (+1 more min ⇒ +15m total, < 16m) and sweep.
	clk.Advance(time.Minute) // now t0+15m
	fired, err := srv.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fired != 1 {
		t.Fatalf("remediation sweep fired %d want 1", fired)
	}
	elapsed := clk.Now().Sub(t0)
	if elapsed >= 16*time.Minute {
		t.Fatalf("remediation fired at %v (>= 16m)", elapsed)
	}
	assertStatus(t, srv.mux(), id, "CANCELLED")

	// EXACTLY ONCE: the void ran once, and a second sweep finds nothing due.
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("remediation void count %d want 1", void)
	}
	if n, _ := srv.sweeper.SweepOnce(ctx); n != 0 {
		t.Fatalf("second sweep fired %d want 0 (not exactly-once)", n)
	}
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("void count %d after second sweep want 1 (double-void!)", void)
	}
	firedRows, _ := srv.st.timerCountByStatus(ctx, "FIRED")
	t.Logf("REMEDIATION: PAYMENT_PENDING auto-voided+cancelled at %v (< 16m), exactly once (void=1, timers FIRED=%d)", elapsed, firedRows)
}

// TestRemediation_NotFiredWhenPaid: if payment.authorized arrives before the
// remediation horizon, the order moves to PAID and the remediation timer is
// cancelled — it must NOT void a paid order.
func TestRemediation_NotFiredWhenPaid(t *testing.T) {
	srv, clk, pay := newTestServer(t)
	ctx := context.Background()
	id := checkout(t, srv, "k")
	inject(t, srv, id, "payment.authorized", "evt-a")
	assertStatus(t, srv.mux(), id, "PAID")

	// The remediation timer should now be CANCELLED (not PENDING).
	pend, _ := srv.st.timerCountByStatus(ctx, "PENDING")
	// After PAID, remediation is cancelled and T_accept is armed ⇒ exactly 1 pending.
	if pend != 1 {
		t.Fatalf("pending timers after PAID = %d want 1 (T_accept only)", pend)
	}

	clk.Advance(20 * time.Minute) // well past the old remediation horizon
	srv.sweeper.SweepOnce(ctx)    // this fires T_accept (no accept happened) → refund path
	// The order is CANCELLED via accept-timeout refund, NOT via a remediation void
	// of a paid order. The remediation timer never fired (it was cancelled).
	if _, _, void, _ := pay.counts(); void != 1 {
		// one void from the accept-timeout refund; the remediation timer added none.
		t.Fatalf("void count %d want 1 (accept-timeout only, no stray remediation void)", void)
	}
	remedFired := remediationFiredCount(t, srv.st, id)
	if remedFired != 0 {
		t.Fatalf("remediation timer fired %d times on a paid order, want 0", remedFired)
	}
}

// remediationFiredCount counts FIRED remediation timers for an order.
func remediationFiredCount(t *testing.T, st *store, orderID string) int {
	t.Helper()
	var n int
	err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM timers WHERE order_id=? AND kind=? AND status='FIRED'`,
		orderID, KindRemediation).Scan(&n)
	if err != nil {
		t.Fatalf("count remediation: %v", err)
	}
	return n
}
