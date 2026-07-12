package main

import (
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
	plane "github.com/shop-platform/shop/services/location-gateway/plane"
)

func newTestServer(t *testing.T, enabled bool) *server {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.close)
	clk := plane.NewManualClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	srv := &server{
		st:     st,
		geo:    plane.NewGeoStore(clk, plane.DefaultTTL),
		tier:   plane.NewTiering(plane.DownsampleRatio),
		broker: eventbus.NewMemBroker(eventbus.WithPartitions(64)),
		clock:  clk,
		log:    logging.New(logging.Config{Service: "location-gateway", Version: "test", Env: "test", Region: "bkk", SampleRate: 1.0}),
		flags:  flags.FromEnv(),
		enabled: enabled, region: "bkk",
	}
	srv.hub = plane.NewHub(plane.HubConfig{Auth: plane.AuthFunc(devAuth), Sink: srv, Clock: clk, BatchWindow: 100 * time.Millisecond})
	return srv
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

// TestFlagGating: with telemetry_v2 OFF every endpoint returns 404 TELEMETRY_DISABLED.
func TestFlagGating(t *testing.T) {
	srv := newTestServer(t, false)
	code, body := do(t, srv, http.MethodPost, "/v1/telemetry:ingest", `{"driver_id":"drv_x","lat":13.75,"lng":100.5}`)
	if code != 404 || !strings.Contains(body, "TELEMETRY_DISABLED") {
		t.Fatalf("flag off: want 404 TELEMETRY_DISABLED, got %d %s", code, body)
	}
	code, body = do(t, srv, http.MethodGet, "/v1/drivers:nearby?lat=13.75&lng=100.5", "")
	if code != 404 || !strings.Contains(body, "TELEMETRY_DISABLED") {
		t.Fatalf("nearby flag off: want 404, got %d %s", code, body)
	}
	if c, _ := do(t, srv, http.MethodGet, "/healthz", ""); c != 200 {
		t.Fatalf("health should be 200, got %d", c)
	}
}

// TestIngestThenKNN: the e2e-shaped path — a simulated driver streams a position,
// then a kNN query returns it (behind telemetry_v2).
func TestIngestThenKNN(t *testing.T) {
	srv := newTestServer(t, true)
	code, body := do(t, srv, http.MethodPost, "/v1/telemetry:ingest",
		`{"driver_id":"drv_e2e_1","lat":13.7460,"lng":100.5340}`)
	if code != 202 || !strings.Contains(body, `"auth_once":true`) {
		t.Fatalf("ingest: want 202 auth_once, got %d %s", code, body)
	}
	code, body = do(t, srv, http.MethodGet, "/v1/drivers:nearby?lat=13.7461&lng=100.5341&k=5", "")
	if code != 200 || !strings.Contains(body, "drv_e2e_1") {
		t.Fatalf("nearby: want the streamed driver, got %d %s", code, body)
	}
	// the returned neighbor should carry an h3_cell (res-7 key)
	if !strings.Contains(body, "h7_") {
		t.Fatalf("nearby result missing h3 cell: %s", body)
	}
}

// TestAuthOnceAcrossIngestCalls: repeated ingests for the same driver re-use the
// same stream — auth happens ONCE, not per call.
func TestAuthOnceAcrossIngestCalls(t *testing.T) {
	srv := newTestServer(t, true)
	for i := 0; i < 20; i++ {
		code, _ := do(t, srv, http.MethodPost, "/v1/driver/positions",
			`{"driver_id":"drv_stream","lat":13.75,"lng":100.53}`)
		if code != 202 {
			t.Fatalf("ingest %d: %d", i, code)
		}
	}
	if got := srv.hub.AuthCount(); got != 1 {
		t.Fatalf("auth-once across ingest calls VIOLATED: %d auth calls for 20 ingests (want 1)", got)
	}
	if srv.hub.OpenStreams() != 1 {
		t.Fatalf("want a single reused stream, got %d", srv.hub.OpenStreams())
	}
}

// TestBadCoords / missing driver rejected.
func TestIngestValidation(t *testing.T) {
	srv := newTestServer(t, true)
	code, body := do(t, srv, http.MethodPost, "/v1/telemetry:ingest", `{"lat":13.75,"lng":100.5}`)
	if code != 400 || !strings.Contains(body, "DRIVER_ID_REQUIRED") {
		t.Fatalf("missing driver: want 400 DRIVER_ID_REQUIRED, got %d %s", code, body)
	}
	code, body = do(t, srv, http.MethodPost, "/v1/telemetry:ingest", `{"driver_id":"drv_x"}`)
	if code != 400 || !strings.Contains(body, "BAD_COORDS") {
		t.Fatalf("missing coords: want 400 BAD_COORDS, got %d %s", code, body)
	}
}

// TestCloseTripWritesOnePGRow: closing a trip writes exactly one summary row.
func TestCloseTripWritesOnePGRow(t *testing.T) {
	srv := newTestServer(t, true)
	// stream some raw frames first (these must NOT create PG rows)
	for i := 0; i < 50; i++ {
		do(t, srv, http.MethodPost, "/v1/telemetry:ingest", `{"driver_id":"drv_trip","lat":13.75,"lng":100.53}`)
	}
	n, _ := srv.st.tripSummaryCount(context.Background())
	if n != 0 {
		t.Fatalf("raw frames wrote %d PG rows (want 0 — raw never hits PG)", n)
	}
	code, body := do(t, srv, http.MethodPost, "/v1/trips:close",
		`{"driver_id":"drv_trip","order_id":"ord_1","start_lat":13.75,"start_lng":100.53,"end_lat":13.76,"end_lng":100.54}`)
	if code != 201 || !strings.Contains(body, `"pg_row_written":1`) {
		t.Fatalf("close trip: want 201 one row, got %d %s", code, body)
	}
	n, _ = srv.st.tripSummaryCount(context.Background())
	if n != 1 {
		t.Fatalf("trip summary rows: got %d want 1", n)
	}
}

// TestGeoStatsReportsSkew: the admin stats surface the hottest-key fraction the
// ingest/skew dashboards read.
func TestGeoStatsReportsSkew(t *testing.T) {
	srv := newTestServer(t, true)
	for i := 0; i < 200; i++ {
		id := "drv_" + string(rune('a'+i%20))
		do(t, srv, http.MethodPost, "/v1/telemetry:ingest",
			`{"driver_id":"`+id+`","lat":13.746,"lng":100.534}`)
	}
	code, body := do(t, srv, http.MethodGet, "/v1/admin/geo/stats", "")
	if code != 200 {
		t.Fatalf("geo stats: %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("stats not JSON: %v", err)
	}
	if _, ok := got["hottest_key_fraction"]; !ok {
		t.Fatalf("stats missing hottest_key_fraction: %s", body)
	}
	if got["produce_errors"].(float64) != 0 {
		t.Fatalf("produce errors non-zero: %v", got["produce_errors"])
	}
}
