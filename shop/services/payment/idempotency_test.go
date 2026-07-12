package main

import (
	"context"
	"sync"
	"testing"
)

// idempotency_test.go is the D9 flagship: PG-durable, transaction-durable
// idempotency on EVERY money mutation. authorize / capture / refund are each
// guarded by the UNIQUE(idempotency_key) insert committed IN THE SAME
// transaction as the money effect, so a retried mutation (same Idempotency-Key)
// is exactly-once at the DB level — proven by charge / capture / refund counts.
// Everything runs under `-race`.

// TestAuthorize_ExactlyOneCharge_DoubleTapRetry: two concurrent "Pay" taps + a
// BFF retry, all with the SAME Idempotency-Key ⇒ ONE payment row, ONE PSP charge,
// ONE payment.authorized event. The redundant submissions replay the winner.
func TestAuthorize_ExactlyOneCharge_DoubleTapRetry(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()
	const key = "pay-tap-key-ABC"
	body := authBody("ord_tap", goodCard)

	// Two concurrent taps with the same key.
	ids := make([]string, 3)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, m := do(t, h, "POST", "/v1/payments:authorize", body, key)
			ids[i], _ = m["payment_id"].(string)
		}(i)
	}
	wg.Wait()
	// A BFF retry with the same key (sequential).
	_, m := do(t, h, "POST", "/v1/payments:authorize", body, key)
	ids[2], _ = m["payment_id"].(string)

	// EXACTLY ONE payment row.
	if n, _ := srv.st.paymentCount(ctx); n != 1 {
		t.Fatalf("payment count %d want 1 (double-tap + retry created duplicate charges!)", n)
	}
	// All successful responses reference the SAME payment_id.
	var pid string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if pid == "" {
			pid = id
		} else if id != pid {
			t.Fatalf("responses disagree on payment_id: %q vs %q", pid, id)
		}
	}
	if pid == "" {
		t.Fatalf("no successful authorize among %v", ids)
	}
	// EXACTLY ONE PSP charge, ONE payment.authorized event.
	if a, _, _ := psp.counts(); a != 1 {
		t.Fatalf("PSP charge count %d want 1 across tap+tap+retry", a)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.authorized"); oc != 1 {
		t.Fatalf("payment.authorized outbox %d want 1", oc)
	}
	t.Logf("D9 EXACTLY-ONE-CHARGE: rows=1, PSP charges=1, payment.authorized=1 under tap+tap+retry")
}

// TestAuthorize_ReplayHeader: a repeat authorize with the same key replays the
// stored 201 with Idempotency-Replayed: true (02 §3) and does NOT re-charge.
func TestAuthorize_ReplayHeader(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	body := authBody("ord_replay", goodCard)
	code1, m1, _ := doResp(t, h, "POST", "/v1/payments:authorize", body, "rk")
	if code1 != 201 {
		t.Fatalf("first authorize %d", code1)
	}
	code2, m2, hdr2 := doResp(t, h, "POST", "/v1/payments:authorize", body, "rk")
	if code2 != 201 {
		t.Fatalf("replay authorize %d", code2)
	}
	if hdr2.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay missing Idempotency-Replayed header")
	}
	if m1["payment_id"] != m2["payment_id"] {
		t.Fatalf("replay returned different payment_id")
	}
	if a, _, _ := psp.counts(); a != 1 {
		t.Fatalf("replay re-charged: PSP charge count %d want 1", a)
	}
}

// TestAuthorize_KeyReuse_409: the same key with a DIFFERENT body ⇒ 409
// IDEMPOTENCY_KEY_REUSED (02 §3), no second charge.
func TestAuthorize_KeyReuse_409(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	if code, _ := do(t, h, "POST", "/v1/payments:authorize", authBody("ord_a", goodCard), "dup"); code != 201 {
		t.Fatalf("first authorize")
	}
	code, m := do(t, h, "POST", "/v1/payments:authorize", authBody("ord_b", goodCard), "dup")
	if code != 409 || errCode(m) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse -> %d %s (want 409 IDEMPOTENCY_KEY_REUSED)", code, errCode(m))
	}
	if a, _, _ := psp.counts(); a != 1 {
		t.Fatalf("key-reuse charged twice: %d want 1", a)
	}
}

// TestCapture_ExactlyOnce_Retry: concurrent captures + a retry with the SAME
// Idempotency-Key ⇒ exactly ONE PSP capture, ONE payment.captured event.
func TestCapture_ExactlyOnce_Retry(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()
	id := authorize(t, srv, "a", "ord_cap", goodCard)
	const key = "cap-key"

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(t, h, "POST", "/v1/payments/"+id+":capture", "", key)
		}()
	}
	wg.Wait()
	do(t, h, "POST", "/v1/payments/"+id+":capture", "", key) // retry

	if _, c, _ := psp.counts(); c != 1 {
		t.Fatalf("PSP capture count %d want 1 (retry double-captured!)", c)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.captured"); oc != 1 {
		t.Fatalf("payment.captured outbox %d want 1", oc)
	}
	if n, _ := srv.st.transitionCount(ctx, id, "capture:api"); n != 1 {
		t.Fatalf("capture:api transitions %d want 1", n)
	}
}

// TestRefund_ExactlyOnce_Retry: concurrent refunds + a retry with the SAME key ⇒
// exactly ONE PSP refund, ONE payment.refunded event.
func TestRefund_ExactlyOnce_Retry(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()
	id := authorize(t, srv, "a", "ord_ref", goodCard)
	do(t, h, "POST", "/v1/payments/"+id+":capture", "", "cap")
	const key = "ref-key"

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(t, h, "POST", "/v1/payments/"+id+":refund", "", key)
		}()
	}
	wg.Wait()
	do(t, h, "POST", "/v1/payments/"+id+":refund", "", key) // retry

	if _, _, r := psp.counts(); r != 1 {
		t.Fatalf("PSP refund count %d want 1 (retry double-refunded!)", r)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.refunded"); oc != 1 {
		t.Fatalf("payment.refunded outbox %d want 1", oc)
	}
}
