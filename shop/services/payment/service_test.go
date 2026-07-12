package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
)

// --- test harness -----------------------------------------------------------

// t0 pins the injected clock (frozen; tests Advance it, never sleep).
var t0 = time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)

// newTestServer builds an in-process payment server on in-memory SQLite with a
// ManualClock and a COUNTING PSP adapter (so a test asserts "exactly one charge"
// by counting Authorize calls), payment_v1 forced on. No Docker, no external DB,
// no Kafka, no real acquirer — the D9 idempotency path, the SwappableCache
// (droppable to simulate Redis failover), the exactly-once inbox, and the money
// state machine are the real code paths.
func newTestServer(t *testing.T) (*server, *ManualClock, *countingPSP) {
	t.Helper()
	ctx := context.Background()
	clk := NewManualClock(t0)
	st, err := openStore(ctx, "bkk", clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(st.close)
	psp := newCountingPSP()
	pm := newPayments(st, psp, "bkk", "") // no webhook callback in unit tests
	srv := &server{
		st: st, pm: pm,
		webhooks: newWebhookConsumer(st, clk),
		orders:   newOrderConsumer(pm, st, clk),
		clock:    clk,
		log:      logging.New(logging.Config{Service: "payment", Version: "test", Env: "test", Region: "bkk", SampleRate: 0, Out: &bytes.Buffer{}}),
		flags:    flags.NewSet(map[string]string{"payment_v1": "true"}),
		enabled:  true, region: "bkk", admin: true,
	}
	return srv, clk, psp
}

// do issues a request against the mux and returns (status, body-map). key sets
// the Idempotency-Key header when non-empty.
func do(t *testing.T, h http.Handler, method, path, body, key string) (int, map[string]any) {
	t.Helper()
	code, m, _ := doResp(t, h, method, path, body, key)
	return code, m
}

// doResp is do but also returns the response header (for Idempotency-Replayed).
func doResp(t *testing.T, h http.Handler, method, path, body, key string) (int, map[string]any, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Result().Header
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

// authBody builds an authorize request body for an order + card.
func authBody(orderID, card string) string {
	return fmt.Sprintf(`{"order_id":%q,"customer_id":"usr_test","amount":{"amount":42550,"currency":"THB"},"card_number":%q}`, orderID, card)
}

// authorize creates one payment and returns its id (fails the test on non-201).
func authorize(t *testing.T, srv *server, key, orderID, card string) string {
	t.Helper()
	code, m := do(t, srv.mux(), "POST", "/v1/payments:authorize", authBody(orderID, card), key)
	if code != 201 {
		t.Fatalf("authorize %s -> %d %v (want 201)", orderID, code, m)
	}
	id, _ := m["payment_id"].(string)
	if id == "" {
		t.Fatalf("authorize returned no payment_id: %v", m)
	}
	return id
}

const goodCard = "4111111111111111"

// --- basic endpoint tests ---------------------------------------------------

// TestAuthorize_Creates_Authorized: POST :authorize ⇒ 201 AUTHORIZED, one payment
// row, one payment.authorized event, exactly one PSP charge.
func TestAuthorize_Creates_Authorized(t *testing.T) {
	srv, _, psp := newTestServer(t)
	ctx := context.Background()
	code, m := do(t, srv.mux(), "POST", "/v1/payments:authorize", authBody("ord_a1", goodCard), "k1")
	if code != 201 || m["status"] != "AUTHORIZED" {
		t.Fatalf("authorize -> %d %v (want 201 AUTHORIZED)", code, m)
	}
	if id, _ := m["payment_id"].(string); !strings.HasPrefix(id, "pay_") {
		t.Fatalf("bad payment_id %v", m["payment_id"])
	}
	if n, _ := srv.st.paymentCount(ctx); n != 1 {
		t.Fatalf("payment count %d want 1", n)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.authorized"); oc != 1 {
		t.Fatalf("payment.authorized outbox %d want 1", oc)
	}
	if a, _, _ := psp.counts(); a != 1 {
		t.Fatalf("PSP authorize (charge) count %d want 1", a)
	}
}

// TestAuthorize_RequiresKey: a money mutation with no Idempotency-Key ⇒ 400.
func TestAuthorize_RequiresKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, m := do(t, srv.mux(), "POST", "/v1/payments:authorize", authBody("ord_x", goodCard), "")
	if code != 400 || errCode(m) != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("no key -> %d %s (want 400 IDEMPOTENCY_KEY_REQUIRED)", code, errCode(m))
	}
}

// TestAuthorize_MissingOrder: order_id required ⇒ 400 VALIDATION.
func TestAuthorize_MissingOrder(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, m := do(t, srv.mux(), "POST", "/v1/payments:authorize", `{"amount":{"amount":1,"currency":"THB"}}`, "k")
	if code != 400 || errCode(m) != "VALIDATION" {
		t.Fatalf("missing order -> %d %s", code, errCode(m))
	}
}

// TestFlagGate: payment_v1 off ⇒ authorize disabled (404 PAYMENT_DISABLED).
func TestFlagGate(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.flags = flags.NewSet(map[string]string{"payment_v1": "false"})
	srv.enabled = false
	code, m := do(t, srv.mux(), "POST", "/v1/payments:authorize", authBody("ord_a", goodCard), "k")
	if code != 404 || errCode(m) != "PAYMENT_DISABLED" {
		t.Fatalf("flag off -> %d %s (want 404 PAYMENT_DISABLED)", code, errCode(m))
	}
}

// TestGetPayment: detail read + unknown ⇒ 404.
func TestGetPayment(t *testing.T) {
	srv, _, _ := newTestServer(t)
	id := authorize(t, srv, "k1", "ord_g", goodCard)
	code, m := do(t, srv.mux(), "GET", "/v1/payments/"+id, "", "")
	if code != 200 || m["payment_id"] != id {
		t.Fatalf("get -> %d %v", code, m)
	}
	code, m = do(t, srv.mux(), "GET", "/v1/payments/pay_missing", "", "")
	if code != 404 || errCode(m) != "PAYMENT_NOT_FOUND" {
		t.Fatalf("get unknown -> %d %s", code, errCode(m))
	}
}

// TestFullHappyPath: authorize → capture → refund through the real HTTP surface,
// with exact PSP effect counts and the fold-over-events == row status (01 §6).
func TestFullHappyPath(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()
	id := authorize(t, srv, "auth-k", "ord_happy", goodCard)

	code, m := do(t, h, "POST", "/v1/payments/"+id+":capture", "", "cap-k")
	if code != 200 || m["status"] != "CAPTURED" {
		t.Fatalf("capture -> %d %v (want 200 CAPTURED)", code, m)
	}
	code, m = do(t, h, "POST", "/v1/payments/"+id+":refund", "", "ref-k")
	if code != 200 || m["status"] != "REFUNDED" {
		t.Fatalf("refund -> %d %v (want 200 REFUNDED)", code, m)
	}
	a, c, r := psp.counts()
	if a != 1 || c != 1 || r != 1 {
		t.Fatalf("PSP counts auth=%d cap=%d ref=%d want 1/1/1", a, c, r)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.captured"); oc != 1 {
		t.Fatalf("payment.captured outbox %d want 1", oc)
	}
	if oc, _ := srv.st.outboxCountTopic(ctx, "payment.refunded"); oc != 1 {
		t.Fatalf("payment.refunded outbox %d want 1", oc)
	}
	// The fold over payment_events reconstructs the same terminal status.
	folded, _ := srv.st.FoldStatus(ctx, id)
	if folded != StateRefunded {
		t.Fatalf("fold -> %s want REFUNDED", folded)
	}
}

// TestCapture_WrongState_409: capturing an already-refunded payment ⇒ 409.
func TestCapture_WrongState_409(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.mux()
	id := authorize(t, srv, "a", "ord_ws", goodCard)
	do(t, h, "POST", "/v1/payments/"+id+":capture", "", "c")
	do(t, h, "POST", "/v1/payments/"+id+":refund", "", "r")
	code, m := do(t, h, "POST", "/v1/payments/"+id+":capture", "", "c2")
	if code != 409 || errCode(m) != "PAYMENT_INVALID_TRANSITION" {
		t.Fatalf("capture refunded -> %d %s (want 409 PAYMENT_INVALID_TRANSITION)", code, errCode(m))
	}
}

// TestOrderConflict_409: a second authorize for the same order under a DIFFERENT
// key ⇒ 409 PAYMENT_ORDER_CONFLICT, no second charge (UNIQUE(order_id) guard).
func TestOrderConflict_409(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	authorize(t, srv, "k1", "ord_dup", goodCard)
	code, m := do(t, h, "POST", "/v1/payments:authorize", authBody("ord_dup", goodCard), "k2")
	if code != 409 || errCode(m) != "PAYMENT_ORDER_CONFLICT" {
		t.Fatalf("dup order -> %d %s (want 409 PAYMENT_ORDER_CONFLICT)", code, errCode(m))
	}
	if a, _, _ := psp.counts(); a != 1 {
		t.Fatalf("charge count %d want 1 (a duplicate order double-charged!)", a)
	}
}

// TestWallet_Credit_And_Pay: credit a wallet, then a wallet-funded authorize
// debits it exactly once (D9).
func TestWallet_Credit_And_Pay(t *testing.T) {
	srv, _, psp := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()
	if code, _ := do(t, h, "POST", "/v1/wallet:credit", `{"customer_id":"usr_w","amount":{"amount":100000,"currency":"THB"}}`, "credit-1"); code != 200 {
		t.Fatalf("credit -> %d", code)
	}
	body := `{"order_id":"ord_wallet","customer_id":"usr_w","method":"wallet","amount":{"amount":42550,"currency":"THB"}}`
	code, m := do(t, h, "POST", "/v1/payments:authorize", body, "wpay-1")
	if code != 201 || m["status"] != "AUTHORIZED" {
		t.Fatalf("wallet authorize -> %d %v", code, m)
	}
	// No PSP charge for a wallet payment.
	if a, _, _ := psp.counts(); a != 0 {
		t.Fatalf("wallet pay hit the PSP %d times want 0", a)
	}
	// Balance debited exactly once: 100000 - 42550 = 57450.
	bal, _, _ := srv.st.walletBalance(ctx, "usr_w")
	if bal.Amount != 57450 {
		t.Fatalf("wallet balance %d want 57450", bal.Amount)
	}
	// Insufficient funds on a second large wallet pay ⇒ 422.
	body2 := `{"order_id":"ord_wallet2","customer_id":"usr_w","method":"wallet","amount":{"amount":90000,"currency":"THB"}}`
	code, m = do(t, h, "POST", "/v1/payments:authorize", body2, "wpay-2")
	if code != 422 || errCode(m) != "WALLET_INSUFFICIENT_FUNDS" {
		t.Fatalf("overdraw -> %d %s (want 422 WALLET_INSUFFICIENT_FUNDS)", code, errCode(m))
	}
}
