package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
)

// --- test harness -----------------------------------------------------------

// t0 pins the injected clock (frozen; tests Advance it, never sleep).
var t0 = time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)

// newTestServer builds an in-process order server on in-memory SQLite with a
// ManualClock, a COUNTING payment client (so a test asserts "exactly one
// charge"), and saga_v1 forced on. No Docker, no external DB, no Kafka — the
// state machine, the durable-timer sweeper, the D9 idempotency path, and the
// exactly-once inbox are the real code paths.
func newTestServer(t *testing.T) (*server, *ManualClock, *countingPaymentClient) {
	t.Helper()
	ctx := context.Background()
	clk := NewManualClock(t0)
	st, err := openStore(ctx, "bkk", clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(st.close)
	pay := newCountingPaymentClient()
	sg := newSaga(st, pay, "bkk")
	consumer := newSagaConsumer(sg, st, clk)
	sweeper := NewSweeper(st, "test-sweeper", clk, func(ctx context.Context, tm TimerRow) error {
		_, _, err := sg.ApplyTrigger(ctx, tm.OrderID, tm.Trigger, map[string]any{"timer": tm.Kind}, clk.Now())
		if err != nil && !isInvalidTransition(err) && !isNotFound(err) {
			return err
		}
		return nil
	})
	srv := &server{
		st: st, sg: sg, consumer: consumer, sweeper: sweeper, clock: clk,
		log:     logging.New(logging.Config{Service: "order", Version: "test", Env: "test", Region: "bkk", SampleRate: 0, Out: &bytes.Buffer{}}),
		flags:   flags.NewSet(map[string]string{"saga_v1": "true"}),
		enabled: true, region: "bkk", admin: true,
	}
	return srv, clk, pay
}

// do issues a request against the mux and returns (status, body-map). key sets
// the Idempotency-Key header when non-empty.
func do(t *testing.T, h http.Handler, method, path, body, key string) (int, map[string]any) {
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
	return rec.Code, m
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

const checkoutBody = `{"quote_id":"qot_test","payment_method_id":"pm_test"}`

// checkout is a helper that creates one order and returns its id.
func checkout(t *testing.T, srv *server, key string) string {
	t.Helper()
	code, m := do(t, srv.mux(), "POST", "/v1/orders", checkoutBody, key)
	if code != 201 {
		t.Fatalf("checkout -> %d %v", code, m)
	}
	id, _ := m["order_id"].(string)
	if id == "" {
		t.Fatalf("checkout returned no order_id: %v", m)
	}
	return id
}

// --- basic endpoint tests ---------------------------------------------------

// TestCheckout_Creates_PaymentPending: POST /v1/orders returns 201 PAYMENT_PENDING
// with an order_id, arms the remediation timer, emits order.created, and requests
// exactly one authorization.
func TestCheckout_Creates_PaymentPending(t *testing.T) {
	srv, _, pay := newTestServer(t)
	code, m := do(t, srv.mux(), "POST", "/v1/orders", checkoutBody, "idem-1")
	if code != 201 || m["status"] != "PAYMENT_PENDING" {
		t.Fatalf("checkout -> %d %v (want 201 PAYMENT_PENDING)", code, m)
	}
	id := m["order_id"].(string)
	if !strings.HasPrefix(id, "ord_") {
		t.Fatalf("bad order_id %q", id)
	}
	// one order, one order.created event, one remediation timer, one authorize.
	n, _ := srv.st.orderCount(context.Background())
	if n != 1 {
		t.Fatalf("order count %d want 1", n)
	}
	oc, _ := srv.st.outboxCountTopic(context.Background(), "order.created")
	if oc != 1 {
		t.Fatalf("order.created outbox %d want 1", oc)
	}
	pend, _ := srv.st.timerCountByStatus(context.Background(), "PENDING")
	if pend != 1 {
		t.Fatalf("pending timers %d want 1 (remediation)", pend)
	}
	if auth, _, _, _ := pay.counts(); auth != 1 {
		t.Fatalf("authorize count %d want 1", auth)
	}
}

// TestCheckout_RequiresKey: a checkout with no Idempotency-Key is rejected 400.
func TestCheckout_RequiresKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, m := do(t, srv.mux(), "POST", "/v1/orders", checkoutBody, "")
	if code != 400 || errCode(m) != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("no key -> %d %s (want 400 IDEMPOTENCY_KEY_REQUIRED)", code, errCode(m))
	}
}

// TestCheckout_MissingQuote: quote_id required -> 400 VALIDATION.
func TestCheckout_MissingQuote(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, m := do(t, srv.mux(), "POST", "/v1/orders", `{"payment_method_id":"pm"}`, "idem-x")
	if code != 400 || errCode(m) != "VALIDATION" {
		t.Fatalf("missing quote -> %d %s", code, errCode(m))
	}
}

// TestFlagGate: saga_v1 off -> checkout disabled (404 SAGA_DISABLED). Ships dark.
func TestFlagGate(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.flags = flags.NewSet(map[string]string{"saga_v1": "false"})
	srv.enabled = false
	code, m := do(t, srv.mux(), "POST", "/v1/orders", checkoutBody, "idem-1")
	if code != 404 || errCode(m) != "SAGA_DISABLED" {
		t.Fatalf("flag off -> %d %s (want 404 SAGA_DISABLED)", code, errCode(m))
	}
}

// TestGetOrder: detail read + unknown -> 404.
func TestGetOrder(t *testing.T) {
	srv, _, _ := newTestServer(t)
	id := checkout(t, srv, "idem-1")
	code, m := do(t, srv.mux(), "GET", "/v1/orders/"+id, "", "")
	if code != 200 || m["order_id"] != id {
		t.Fatalf("get -> %d %v", code, m)
	}
	code, m = do(t, srv.mux(), "GET", "/v1/orders/ord_missing", "", "")
	if code != 404 || errCode(m) != "ORDER_NOT_FOUND" {
		t.Fatalf("get unknown -> %d %s", code, errCode(m))
	}
}

// TestCancel_FromPaymentPending: :cancel from PAYMENT_PENDING -> CANCELLED (void).
func TestCancel_FromPaymentPending(t *testing.T) {
	srv, _, pay := newTestServer(t)
	id := checkout(t, srv, "idem-1")
	code, m := do(t, srv.mux(), "POST", "/v1/orders/"+id+":cancel", "{}", "")
	if code != 200 || m["status"] != "CANCELLED" {
		t.Fatalf("cancel -> %d %v", code, m)
	}
	// void ran once (the held auth from checkout).
	if _, _, void, _ := pay.counts(); void != 1 {
		t.Fatalf("void count %d want 1", void)
	}
	// terminal -> pending timers cancelled.
	pend, _ := srv.st.timerCountByStatus(context.Background(), "PENDING")
	if pend != 0 {
		t.Fatalf("pending timers %d want 0 after cancel", pend)
	}
}

// TestFullHappyPath drives checkout -> payment.authorized -> accept -> dispatch ->
// pickup -> delivered -> settle through the real service surface, asserting each
// state + the fold-over-events equals the row status (01 §6).
func TestFullHappyPath(t *testing.T) {
	srv, _, pay := newTestServer(t)
	h := srv.mux()
	id := checkout(t, srv, "idem-happy")

	inject(t, srv, id, "payment.authorized", "evt-pay-1")
	assertStatus(t, h, id, "PAID")
	do(t, h, "POST", "/v1/orders/"+id+":accept", "{}", "")
	assertStatus(t, h, id, "ACCEPTED")
	inject(t, srv, id, "dispatch.assigned", "evt-dsp-1")
	assertStatus(t, h, id, "DISPATCHED")
	inject(t, srv, id, "driver.picked_up", "evt-pick-1")
	assertStatus(t, h, id, "PICKED_UP")
	inject(t, srv, id, "driver.delivered", "evt-deliv-1")
	assertStatus(t, h, id, "DELIVERED")

	// DELIVERED -> SETTLED via capture-by timer: advance clock, sweep.
	srv.clock.(*ManualClock).Advance(DefaultCaptureByWindow + time.Minute)
	if _, err := srv.sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	assertStatus(t, h, id, "SETTLED")
	if _, cap, _, _ := pay.counts(); cap != 1 {
		t.Fatalf("capture count %d want 1", cap)
	}
	// The fold over order_events reconstructs the same terminal state (01 §6).
	folded, _ := srv.st.FoldState(context.Background(), id)
	if folded != StateSettled {
		t.Fatalf("fold -> %s want SETTLED", folded)
	}
}

// --- shared assertions ------------------------------------------------------

func assertStatus(t *testing.T, h http.Handler, id, want string) {
	t.Helper()
	code, m := do(t, h, "GET", "/v1/orders/"+id, "", "")
	if code != 200 || m["status"] != want {
		t.Fatalf("order %s status %v want %s (code %d)", id, m["status"], want, code)
	}
}

// inject delivers a domain event envelope through the consumer (exactly-once
// inbox path), with an explicit event_id so a test can redeliver the same one.
func inject(t *testing.T, srv *server, orderID, eventType, eventID string) {
	t.Helper()
	env, err := makeDomainEnvelope(eventID, eventType, orderID, "bkk", map[string]any{"order_id": orderID}, srv.clock.Now())
	if err != nil {
		t.Fatalf("makeDomainEnvelope: %v", err)
	}
	if _, err := srv.consumer.InjectEnvelope(context.Background(), env); err != nil {
		t.Fatalf("inject %s: %v", eventType, err)
	}
}

var _ = eventbus.Message{}
