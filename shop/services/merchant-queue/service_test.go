package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
)

// --- test harness -----------------------------------------------------------

// fakeOrderClient records accept/reject calls and returns a scripted outcome so a
// test can drive "accept → saga proceeds" without a live order service.
type fakeOrderClient struct {
	mu      sync.Mutex
	accepts int
	rejects int
	outcome acceptOutcome // zero value = acceptOK
}

func (f *fakeOrderClient) Accept(context.Context, string) acceptOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepts++
	return f.outcome
}
func (f *fakeOrderClient) Reject(context.Context, string) acceptOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejects++
	return f.outcome
}
func (f *fakeOrderClient) acceptCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.accepts }

var testBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T) (*server, *ManualClock, *fakeOrderClient) {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx, "bkk")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.close)
	clk := NewManualClock(testBase)
	foc := &fakeOrderClient{}
	srv := &server{
		st:      st,
		pr:      newProjection(st, clk),
		adm:     newAdmission(),
		orders:  foc,
		clock:   clk,
		log:     logging.New(logging.Config{Service: "merchant-queue", Version: "test", Env: "test", Region: "bkk", SampleRate: 1.0}),
		flags:   flags.FromEnv(),
		enabled: true,
		region:  "bkk",
	}
	return srv, clk, foc
}

// injectEvent pushes an order.* event through the projection (the E2E stub path).
func (s *server) injectEvent(t *testing.T, eventID, eventType, orderID, merchantID string, at time.Time, extra map[string]any) {
	t.Helper()
	env, err := makeOrderEnvelope(eventID, eventType, orderID, merchantID, "bkk", extra, at)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if _, err := s.pr.InjectEnvelope(context.Background(), env); err != nil {
		t.Fatalf("inject %s: %v", eventType, err)
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// --- tests ------------------------------------------------------------------

// TestFlagGating: with merchant_queue_v1 OFF every gated endpoint is 404
// MERCHANT_QUEUE_DISABLED; with it ON they work.
func TestFlagGating(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.enabled = false
	h := srv.mux()
	rec, out := do(t, h, http.MethodGet, "/v1/merchant/orders?merchant_id=mer_x", "")
	if rec.Code != 404 {
		t.Fatalf("disabled list -> %d, want 404", rec.Code)
	}
	if code, _ := errCode(out); code != "MERCHANT_QUEUE_DISABLED" {
		t.Fatalf("want MERCHANT_QUEUE_DISABLED, got %v", out)
	}
	// healthz is never gated.
	rec, _ = do(t, h, http.MethodGet, "/healthz", "")
	if rec.Code != 200 {
		t.Fatalf("healthz -> %d", rec.Code)
	}
}

// TestAcceptDrivesSaga: an order in the queue (PAID) → accept via the BFF verb
// consumes a token, CALLS THE ORDER SAGA, and the queue row becomes ACCEPTED.
func TestAcceptDrivesSaga(t *testing.T) {
	srv, _, foc := newTestServer(t)
	h := srv.mux()
	oid, mid := "ord_svc_1", "mer_svc_1"
	srv.injectEvent(t, "evt_c", TopicOrderCreated, oid, "", testBase, nil)
	srv.injectEvent(t, "evt_p", TopicOrderPaid, oid, mid, testBase.Add(3*time.Second), map[string]any{"paid_at": testBase.Format(time.RFC3339)})

	// It shows in the merchant's incoming queue as PENDING.
	_, out := do(t, h, http.MethodGet, "/v1/merchant/orders?merchant_id="+mid, "")
	if int(out["count"].(float64)) != 1 {
		t.Fatalf("queue count = %v, want 1 (%v)", out["count"], out)
	}

	// Accept → saga driven, row ACCEPTED.
	rec, ack := do(t, h, http.MethodPost, "/v1/merchant/orders/"+oid+":accept", "{}")
	if rec.Code != 200 {
		t.Fatalf("accept -> %d (%v)", rec.Code, ack)
	}
	if ack["status"] != "ACCEPTED" {
		t.Fatalf("accept status = %v, want ACCEPTED", ack["status"])
	}
	if foc.acceptCount() != 1 {
		t.Fatalf("order saga accept called %d times, want 1", foc.acceptCount())
	}
	row, ok, _ := srv.st.getRow(context.Background(), oid)
	if !ok || row.QueueState != StateAccepted {
		t.Fatalf("row state = %q, want ACCEPTED", row.QueueState)
	}
	// Idempotent re-accept → still ACCEPTED, no extra saga call.
	rec, _ = do(t, h, http.MethodPost, "/v1/merchant/orders/"+oid+":accept", "{}")
	if rec.Code != 200 || foc.acceptCount() != 1 {
		t.Fatalf("re-accept not idempotent: code=%d saga=%d", rec.Code, foc.acceptCount())
	}
}

// TestAcceptUnknownOrder / not-pending guardrails.
func TestAcceptGuards(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.mux()
	rec, out := do(t, h, http.MethodPost, "/v1/merchant/orders/ord_missing:accept", "{}")
	if rec.Code != 404 {
		t.Fatalf("accept missing -> %d, want 404 (%v)", rec.Code, out)
	}
	// created-but-not-paid → not pending → 409.
	srv.injectEvent(t, "evt_c2", TopicOrderCreated, "ord_np", "", testBase, nil)
	rec, out = do(t, h, http.MethodPost, "/v1/merchant/orders/ord_np:accept", `{"merchant_id":"mer_np"}`)
	if rec.Code != 409 {
		t.Fatalf("accept non-pending -> %d, want 409 (%v)", rec.Code, out)
	}
}

// TestReject drives the saga reject → queue row CANCELLED, ack REJECTED.
func TestReject(t *testing.T) {
	srv, _, foc := newTestServer(t)
	h := srv.mux()
	oid, mid := "ord_rej", "mer_rej"
	srv.injectEvent(t, "evt_p", TopicOrderPaid, oid, mid, testBase, map[string]any{"paid_at": testBase.Format(time.RFC3339)})
	rec, ack := do(t, h, http.MethodPost, "/v1/merchant/orders/"+oid+":reject", "{}")
	if rec.Code != 200 || ack["status"] != "REJECTED" {
		t.Fatalf("reject -> %d %v", rec.Code, ack)
	}
	if foc.rejects != 1 {
		t.Fatalf("saga reject called %d, want 1", foc.rejects)
	}
	row, _, _ := srv.st.getRow(context.Background(), oid)
	if row.QueueState != StateCancelled {
		t.Fatalf("row after reject = %q, want CANCELLED", row.QueueState)
	}
}

// TestCapacityTuning: PUT capacity, GET reflects it.
func TestCapacityTuning(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.mux()
	rec, out := do(t, h, http.MethodPut, "/v1/merchant/mer_cap/capacity", `{"accepts_per_window":50,"window_minutes":10}`)
	if rec.Code != 200 {
		t.Fatalf("put capacity -> %d (%v)", rec.Code, out)
	}
	if int(out["accepts_per_window"].(float64)) != 50 {
		t.Fatalf("capacity = %v, want 50", out["accepts_per_window"])
	}
	rec, out = do(t, h, http.MethodGet, "/v1/merchant/mer_cap/capacity", "")
	if rec.Code != 200 || int(out["accepts_per_window"].(float64)) != 50 {
		t.Fatalf("get capacity = %v", out)
	}
	// invalid capacity rejected.
	rec, _ = do(t, h, http.MethodPut, "/v1/merchant/mer_cap/capacity", `{"accepts_per_window":0}`)
	if rec.Code != 400 {
		t.Fatalf("zero capacity -> %d, want 400", rec.Code)
	}
}

// TestInjectRedeliveryExactlyOnce: a redelivered event_id is a no-op (inbox).
func TestInjectRedeliveryExactlyOnce(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	env, _ := makeOrderEnvelope("evt_once", TopicOrderPaid, "ord_once", "mer_once", "bkk",
		map[string]any{"paid_at": testBase.Format(time.RFC3339)}, testBase)
	msg, _ := eventbus.NewMessage(env.EventType, env)
	if err := srv.pr.Handle(ctx, msg); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	n1, _ := srv.st.logCount(ctx)
	// redeliver the SAME event_id 5×.
	for i := 0; i < 5; i++ {
		if err := srv.pr.Handle(ctx, msg); err != nil {
			t.Fatalf("redeliver: %v", err)
		}
	}
	n2, _ := srv.st.logCount(ctx)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("log count n1=%d n2=%d, want 1/1 (redelivery must be a no-op)", n1, n2)
	}
}

func errCode(out map[string]any) (string, bool) {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return "", false
	}
	c, _ := e["code"].(string)
	return c, true
}
