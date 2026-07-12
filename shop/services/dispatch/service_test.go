package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	match "github.com/shop-platform/shop/services/dispatch/match"
)

var testBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeOrderClient captures dispatch.assigned deliveries (the saga push).
type fakeOrderClient struct{ delivered []eventbus.Envelope }

func (f *fakeOrderClient) Deliver(_ context.Context, env eventbus.Envelope) error {
	f.delivered = append(f.delivered, env)
	return nil
}

func newTestServer(t *testing.T, enabled bool) (*server, *match.ManualClock, *fakeOrderClient) {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx, "bkk")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.close)
	clk := match.NewManualClock(testBase)
	eng := match.NewEngine(match.Config{Clock: clk, TTL: 10 * time.Second, BaseSeed: 1, Partitions: 64})
	foc := &fakeOrderClient{}
	srv := &server{
		st: st, eng: eng, pr: newProjection(eng, st, clk), orders: foc,
		broker: eventbus.NewMemBroker(eventbus.WithPartitions(64)), clock: clk,
		log:     logging.New(logging.Config{Service: "dispatch", Version: "test", Env: "test", Region: "bkk", SampleRate: 1.0}),
		flags:   flags.FromEnv(),
		enabled: enabled, region: "bkk", partN: 64,
	}
	return srv, clk, foc
}

func do(t *testing.T, srv *server, method, path, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// TestFlagGating: with dispatch_batch OFF every endpoint returns 404 DISPATCH_DISABLED.
func TestFlagGating(t *testing.T) {
	srv, _, _ := newTestServer(t, false)
	code, body := do(t, srv, http.MethodPost, "/v1/assignments", `{"order_id":"ord_x"}`)
	if code != 404 || !strings.Contains(body, "DISPATCH_DISABLED") {
		t.Fatalf("flag off: want 404 DISPATCH_DISABLED, got %d %s", code, body)
	}
	// health is always available.
	if c, _ := do(t, srv, http.MethodGet, "/healthz", ""); c != 200 {
		t.Fatalf("health should be 200, got %d", c)
	}
}

// TestAssignCompat: the S-T8 compat path — POST /v1/assignments with no
// pre-registered driver → 201 ASSIGNED (synthetic fallback), idempotent.
func TestAssignCompat(t *testing.T) {
	srv, _, foc := newTestServer(t, true)
	code, body := do(t, srv, http.MethodPost, "/v1/assignments", `{"order_id":"ord_compat"}`)
	if code != 201 || !strings.Contains(body, `"status":"ASSIGNED"`) {
		t.Fatalf("assign compat: want 201 ASSIGNED, got %d %s", code, body)
	}
	if len(foc.delivered) != 1 || foc.delivered[0].EventType != TopicDispatchAssigned {
		t.Fatalf("expected one dispatch.assigned delivered to order, got %+v", foc.delivered)
	}
	// idempotent: same order again → still ASSIGNED, same driver.
	c2, b2 := do(t, srv, http.MethodPost, "/v1/assignments", `{"order_id":"ord_compat"}`)
	if c2 != 201 || !strings.Contains(b2, `"status":"ASSIGNED"`) {
		t.Fatalf("assign idempotent: got %d %s", c2, b2)
	}
}

// TestOfferAcceptE2E: the real batch flow — a located driver + a paid order → tick
// → the driver sees an offer → accept → assigned; dispatch.assigned is delivered
// to the order saga.
func TestOfferAcceptE2E(t *testing.T) {
	srv, _, foc := newTestServer(t, true)
	ctx := context.Background()
	pickup := match.Point{Lat: 13.7563, Lng: 100.5018}
	drv := "drv_e2e_1"
	// 1. driver location (availability).
	loc := fmt.Sprintf(`{"driver_id":%q,"lat":%f,"lng":%f}`, drv, pickup.Lat+0.002, pickup.Lng+0.002)
	if c, _ := do(t, srv, http.MethodPost, "/v1/drivers:location", loc); c != 202 {
		t.Fatalf("driver location: %d", c)
	}
	// 2. paid order (needs-dispatch) with an explicit pickup.
	env, _ := makeEnvelope(TopicOrderPaid, "ord_e2e", "bkk", map[string]any{
		"order_id": "ord_e2e", "merchant_id": "mer_e2e", "pickup": pickup, "paid_at": testBase.Format(time.RFC3339),
	}, testBase)
	if _, err := srv.pr.InjectEnvelope(ctx, env); err != nil {
		t.Fatalf("inject order.paid: %v", err)
	}
	// 3. batch tick → an offer for the driver.
	srv.tickAll(ctx)
	c, b := do(t, srv, http.MethodGet, "/v1/driver/offers?driver_id="+drv, "")
	if c != 200 || !strings.Contains(b, "ord_e2e") {
		t.Fatalf("driver offer: want the offer, got %d %s", c, b)
	}
	// 4. accept → assigned.
	ac, ab := do(t, srv, http.MethodPost, "/v1/driver/offers/ord_e2e:accept", `{}`)
	if ac != 200 || !strings.Contains(ab, `"status":"ASSIGNED"`) || !strings.Contains(ab, drv) {
		t.Fatalf("accept: want 200 ASSIGNED to %s, got %d %s", drv, ac, ab)
	}
	// 5. assignment status.
	gc, gb := do(t, srv, http.MethodGet, "/v1/assignments/ord_e2e", "")
	if gc != 200 || !strings.Contains(gb, `"status":"ASSIGNED"`) {
		t.Fatalf("get assignment: %d %s", gc, gb)
	}
	// 6. dispatch.assigned delivered to the order saga.
	if len(foc.delivered) != 1 || foc.delivered[0].EventType != TopicDispatchAssigned {
		t.Fatalf("expected dispatch.assigned delivered, got %+v", foc.delivered)
	}
}

// TestInjectRedeliveryExactlyOnce: a redelivered order.paid (same event_id) is a
// no-op via the durable inbox — the inbox count stays 1.
func TestInjectRedeliveryExactlyOnce(t *testing.T) {
	srv, _, _ := newTestServer(t, true)
	ctx := context.Background()
	env, _ := makeEnvelope(TopicOrderPaid, "ord_once", "bkk", map[string]any{"order_id": "ord_once", "merchant_id": "mer_once"}, testBase)
	env.EventID = "evt_fixed_once"
	msg, _ := eventbus.NewMessage(env.EventType, env)
	for i := 0; i < 6; i++ {
		if err := srv.pr.Handle(ctx, msg); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	n, err := srv.st.inbx.Count(ctx)
	if err != nil {
		t.Fatalf("inbox count: %v", err)
	}
	if n != 1 {
		t.Fatalf("inbox count = %d after 6 deliveries, want 1 (exactly-once)", n)
	}
}

// TestSnapshotLogQueryableAndReplays: the snapshot log is queryable via the admin
// endpoints and every logged snapshot replays identically.
func TestSnapshotLogQueryableAndReplays(t *testing.T) {
	srv, _, _ := newTestServer(t, true)
	ctx := context.Background()
	// populate a few orders + drivers and tick.
	for i := 0; i < 8; i++ {
		p := match.Point{Lat: 13.75 + float64(i)*0.01, Lng: 100.5}
		env, _ := makeEnvelope(TopicOrderPaid, fmt.Sprintf("ord_%02d", i), "bkk", map[string]any{"order_id": fmt.Sprintf("ord_%02d", i), "pickup": p}, testBase)
		_, _ = srv.pr.InjectEnvelope(ctx, env)
		srv.eng.AddDriver(match.Driver{DriverID: fmt.Sprintf("drv_%02d", i), Loc: match.Point{Lat: p.Lat + 0.001, Lng: p.Lng + 0.001}})
	}
	srv.tickAll(ctx)

	code, body := do(t, srv, http.MethodGet, "/v1/admin/snapshots?limit=100", "")
	if code != 200 {
		t.Fatalf("list snapshots: %d", code)
	}
	var listed struct {
		Snapshots []SnapshotRow `json:"snapshots"`
		Count     int           `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode snapshots: %v", err)
	}
	if listed.Count == 0 {
		t.Fatal("no snapshots logged/queryable")
	}
	// every snapshot recorded replay_ok=true.
	for _, s := range listed.Snapshots {
		if !s.ReplayOK {
			t.Fatalf("snapshot tick %d not replay_ok", s.TickID)
		}
	}
	// GET one snapshot → replay_identical true.
	gc, gb := do(t, srv, http.MethodGet, fmt.Sprintf("/v1/admin/snapshots/%d", listed.Snapshots[0].TickID), "")
	if gc != 200 || !strings.Contains(gb, `"replay_identical":true`) {
		t.Fatalf("get snapshot replay: %d %s", gc, gb)
	}
}

// TestReservationsEndpoint: the admin ledger endpoint reports zero leak.
func TestReservationsEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t, true)
	do(t, srv, http.MethodPost, "/v1/assignments", `{"order_id":"ord_r1"}`)
	c, b := do(t, srv, http.MethodGet, "/v1/admin/reservations", "")
	if c != 200 || !strings.Contains(b, `"leak_rate":0`) {
		t.Fatalf("reservations: want leak_rate 0, got %d %s", c, b)
	}
}
