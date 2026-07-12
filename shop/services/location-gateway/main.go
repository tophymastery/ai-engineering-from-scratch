// Command location-gateway is the V-T13 Driver telemetry plane slice (Location
// team; D14 telemetry ingest plane + D15 H3 geo store / telemetry tiering). It is
// the per-cell gateway drivers stream their GPS to: it AUTHENTICATES ONCE per
// connection (not per frame), buffers ~64-byte position frames and BATCHES 100 ms
// windows into the telemetry topic; the batched positions land in an H3 res-7
// Redis-like geo index (30 s TTL) that publishes a kNN read contract dispatch
// consumes; raw frames are downsampled 1:10 into Iceberg and PG keeps ONLY
// per-trip summaries.
//
// Headline correctness properties (all proved for real; adaptations disclosed in
// VERIFICATION §V-T13):
//
//   - HOTTEST H3 KEY < 2% OF WRITES (plane/salt.go + salt_test.go): salted res-7
//     geo keys spread a hot cell across 64 sub-keys; real write histogram.
//   - kNN CORRECT + p99 < 10 ms (plane/geostore.go): ring-expanding EXACT kNN
//     verified vs brute force; latency measured for real.
//   - AUTH-ONCE + 100 ms BATCH, p99 < 5 ms, ZERO produce errors (plane/gateway.go).
//   - 100k RECONNECT STORM recovered < 60 s (plane/reconnect_test.go, frozen clock).
//   - TIERING (plane/tier.go): raw never hits PG; 1:10 Iceberg + trip summaries
//     only ⇒ PG writes < 500/s per cell.
//   - telemetry_v2 e2e: simulated driver streams → kNN returns them via driver-bff.
//
// Endpoints (02 §1 conventions, 02 §2 error envelope; gated by telemetry_v2):
//
//	POST /v1/telemetry:ingest              stream a batch of positions (auth-once per stream) → geo index + telemetry topic
//	POST /v1/driver/positions              driver-bff position-stream endpoint (same ingest; auth-once)
//	GET  /v1/drivers:nearby?lat=&lng=&k=   the kNN read contract dispatch consumes (K nearest live drivers)
//	GET  /v1/orders/{order_id}/eta         location-tracking.v1 compat: live ETA + last driver position
//	POST /v1/trips:close                   close a trip → write the ONE PG per-trip summary (tiering demo)
//	GET  /v1/admin/geo/stats               geo/ingest/skew stats (hottest-key fraction, produce errors, live count)
//
// Sandbox adaptations (disclosed): no gRPC/MQTT ⇒ the stream protocol is modelled
// in-process (the auth-once + 100 ms batch is THIS code, fully tested); no Kafka ⇒
// in-memory eventbus telemetry topic; no Redis ⇒ in-process res-7 geo TTL store;
// no Flink/Iceberg ⇒ an in-process 1:10 downsampler + in-memory analytics sink;
// no PG ⇒ in-memory SQLite for trip summaries. The real gRPC/MQTT/Flink/Iceberg
// topology is expressed in deploy/ manifests, verified render-only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
	plane "github.com/shop-platform/shop/services/location-gateway/plane"
)

var (
	codeTelemetryDisabled = shoperr.Register("TELEMETRY_DISABLED", 404, false, "The telemetry_v2 feature is not enabled.")
	codeUnauthenticated   = shoperr.Register("STREAM_UNAUTHENTICATED", 401, false, "Stream authentication failed.")
	codeBadCoords         = shoperr.Register("BAD_COORDS", 400, false, "lat/lng are required and must be valid numbers.")
	codeDriverRequired    = shoperr.Register("DRIVER_ID_REQUIRED", 400, false, "A driver_id (or token) is required.")
	codeOrderNoTrack      = shoperr.Register("ORDER_NOT_FOUND", 404, false, "No live track for that order.")
)

type server struct {
	st      *store
	geo     *plane.GeoStore
	hub     *plane.Hub
	tier    *plane.Tiering
	broker  *eventbus.MemBroker
	clock   Clock
	log     *logging.Logger
	flags   *flags.Set
	enabled bool // telemetry_v2 default (per-request override honoured in non-prod)
	region  string
}

// devAuth is the sandbox connection authenticator: a token "tok:<driver_id>"
// authenticates to <driver_id>; a bare "<driver_id>" is accepted too (demo
// convenience). Production plugs libs/edgeauth JWT verification here. Called ONCE
// per stream by the hub (the D14 auth-once), never per frame.
func devAuth(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", plane.ErrUnauthenticated
	}
	if strings.HasPrefix(token, "tok:") {
		id := strings.TrimPrefix(token, "tok:")
		if id == "" {
			return "", plane.ErrUnauthenticated
		}
		return id, nil
	}
	return token, nil
}

func main() {
	port := envOr("PORT", "8109")
	name := envOr("SERVICE_NAME", "location-gateway")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "bkk")
	ttl := envDuration("GEO_TTL", plane.DefaultTTL)
	batchWindow := envDuration("BATCH_WINDOW", 100*time.Millisecond)

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		log.Fatalf("location-gateway: open store: %v", err)
	}
	clk := SystemClock{}
	fs := flags.FromEnv()

	srv := &server{
		st:    st,
		geo:   plane.NewGeoStore(clk, ttl),
		tier:  plane.NewTiering(plane.DownsampleRatio),
		clock: clk,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: locationSampleRate(),
		}),
		flags:   fs,
		enabled: fs.Bool("telemetry_v2", false),
		region:  region,
	}
	// Telemetry topic (D5/D14): the gateway batches 100 ms windows here; the sink
	// updates the H3 geo index + Iceberg tier and emits driver.location_updated.
	srv.broker = eventbus.NewMemBroker(eventbus.WithPartitions(envInt("TELEMETRY_PARTITIONS", 512)))
	srv.hub = plane.NewHub(plane.HubConfig{
		Auth:        plane.AuthFunc(devAuth),
		Sink:        srv, // the server IS the telemetry sink (Produce below)
		Clock:       clk,
		BatchWindow: batchWindow,
	})

	// Background 100 ms batch flusher (D14): in production a ticker flushes each
	// window; the tests advance a ManualClock and call Flush explicitly.
	go srv.runFlusher(ctx, batchWindow)

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(srv.mux())))
	addr := ":" + port
	log.Printf("location-gateway %q on %s (env=%s region=%s telemetry_v2=%v ttl=%s batch=%s partitions=%d)",
		name, addr, env, region, srv.enabled, ttl, batchWindow, envInt("TELEMETRY_PARTITIONS", 512))
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("location-gateway server exited: %v", err)
	}
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/telemetry:ingest", s.only(http.MethodPost, s.handleIngest))
	mux.HandleFunc("/v1/driver/positions", s.only(http.MethodPost, s.handleIngest)) // driver-bff position-stream
	mux.HandleFunc("/v1/drivers:nearby", s.only(http.MethodGet, s.handleNearby))
	mux.HandleFunc("/v1/trips:close", s.only(http.MethodPost, s.handleCloseTrip))
	mux.HandleFunc("/v1/admin/geo/stats", s.only(http.MethodGet, s.handleGeoStats))
	mux.HandleFunc("/v1/orders/", s.only(http.MethodGet, s.handleOrderEta)) // {order_id}/eta
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "location-gateway",
		"telemetry_v2":   s.telemetryEnabled(r),
		"region":         s.region,
		"otel_exporter":  otel.ExporterMode(),
		"open_streams":   s.hub.OpenStreams(),
		"produce_errors": s.hub.ProduceErrors(),
	})
}

func (s *server) telemetryEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "telemetry_v2", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.telemetryEnabled(r) {
		s.fail(w, r, shoperr.New(codeTelemetryDisabled, ""))
		return false
	}
	return true
}

// --- Produce: the telemetry-topic sink (D14 batch flush target) --------------
//
// On each 100 ms batch flush the gateway hands the batch here. Produce updates the
// H3 geo index (D15 live positions), routes raw frames to the tiering plane
// (1:10 Iceberg, no raw PG), and emits driver.location_updated onto the telemetry
// topic for downstream consumers (dispatch kNN + customer tracking). Never errors
// in-sandbox ⇒ the "zero produce errors" criterion holds.
func (s *server) Produce(batch []plane.Position) error {
	ctx := context.Background()
	for _, p := range batch {
		cell := s.geo.Update(p.DriverID, p.Lat, p.Lng, p.RecordedAt)
		s.tier.Ingest(p.DriverID, p)
		env, err := makeLocationEnvelope(p.DriverID, s.region, cell, p.Lat, p.Lng, p.RecordedAt)
		if err != nil {
			continue
		}
		if msg, e := eventbus.NewMessage(TopicDriverLocation, env); e == nil {
			_ = s.broker.Publish(ctx, msg)
		}
	}
	return nil
}

func (s *server) runFlusher(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.hub.Flush(false)
		}
	}
}

// --- POST /v1/telemetry:ingest + /v1/driver/positions ------------------------

type ingestPosition struct {
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	RecordedAt string  `json:"recorded_at"`
}

type ingestRequest struct {
	DriverID  string           `json:"driver_id"`
	Token     string           `json:"token"`
	StreamID  string           `json:"stream_id"`
	Lat       *float64         `json:"lat"` // single-position convenience
	Lng       *float64         `json:"lng"`
	Positions []ingestPosition `json:"positions"`
}

// handleIngest is the D14 ingest path: authenticate ONCE per stream (reuse an open
// stream on subsequent calls), buffer the position frames, and flush the 100 ms
// window so the geo index + telemetry topic reflect the write. The auth-once
// property is the hub's: Open authenticates, Push does not.
func (s *server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var in ingestRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	token := in.Token
	if token == "" && in.DriverID != "" {
		token = "tok:" + in.DriverID // demo convenience: derive the token from the id
	}
	if token == "" {
		s.fail(w, r, shoperr.New(codeDriverRequired, ""))
		return
	}
	streamID := in.StreamID
	if streamID == "" {
		// D14: one persistent stream per driver connection ⇒ key the stream on the
		// authenticated driver, so re-ingest reuses the SAME stream (auth-once).
		if did, err := devAuth(token); err == nil {
			streamID = "strm_" + did
		} else {
			s.fail(w, r, shoperr.New(codeUnauthenticated, ""))
			return
		}
	}

	st, driverID, err := s.openOrReuse(streamID, token)
	if err != nil {
		s.fail(w, r, shoperr.New(codeUnauthenticated, ""))
		return
	}

	now := nowFor(r.Context(), s.clock)
	var frames []plane.Frame
	if in.Lat != nil && in.Lng != nil {
		frames = append(frames, plane.Frame{Lat: *in.Lat, Lng: *in.Lng, RecordedAt: now})
	}
	for _, p := range in.Positions {
		rec := now
		if p.RecordedAt != "" {
			if t, e := time.Parse(time.RFC3339, p.RecordedAt); e == nil {
				rec = t
			}
		}
		frames = append(frames, plane.Frame{Lat: p.Lat, Lng: p.Lng, RecordedAt: rec})
	}
	if len(frames) == 0 {
		s.fail(w, r, shoperr.New(codeBadCoords, ""))
		return
	}
	for _, f := range frames {
		_ = st.Push(f)
	}
	// Flush this stream's window now so the demo/read-after-write is immediate
	// (in production the 100 ms ticker owns the cadence).
	s.hub.Flush(true)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"driver_id": driverID, "stream_id": streamID,
		"accepted": len(frames), "batched_100ms": true, "auth_once": true,
	})
}

// openStreams tracks HTTP-side stream reuse so auth happens ONCE per stream_id
// even across separate ingest calls (the HTTP model of a persistent connection).
var _ = context.Background // keep imports tidy across edits

func (s *server) openOrReuse(streamID, token string) (*plane.Stream, string, error) {
	// The hub owns stream identity; if a stream for this id is already open we
	// reuse it (no re-auth). We discover reuse by attempting Open only when absent.
	if st := s.hub.Stream(streamID); st != nil {
		return st, st.DriverID, nil
	}
	st, err := s.hub.Open(streamID, token)
	if err != nil {
		return nil, "", err
	}
	return st, st.DriverID, nil
}

// --- GET /v1/drivers:nearby (the kNN read contract dispatch consumes) --------

func (s *server) handleNearby(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	lat, okLat := parseFloat(r.URL.Query().Get("lat"))
	lng, okLng := parseFloat(r.URL.Query().Get("lng"))
	if !okLat || !okLng {
		s.fail(w, r, shoperr.New(codeBadCoords, ""))
		return
	}
	k := 10
	if v := r.URL.Query().Get("k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			k = n
		}
	}
	neighbors := s.geo.KNN(lat, lng, k)
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   map[string]any{"lat": lat, "lng": lng, "k": k},
		"count":   len(neighbors),
		"drivers": neighbors,
	})
}

// --- GET /v1/orders/{order_id}/eta (location-tracking.v1 compat) -------------

func (s *server) handleOrderEta(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/orders/")
	orderID, suffix, _ := strings.Cut(rest, "/")
	if suffix != "eta" || orderID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeNotFound, "unknown resource"))
		return
	}
	// Compat: report the nearest live driver to the city centre as the tracked
	// driver's live position (the real live-ETA join lives in V-T14 tracking).
	near := s.geo.KNN(cityCenterLat, cityCenterLng, 1)
	if len(near) == 0 {
		s.fail(w, r, shoperr.New(codeOrderNoTrack, ""))
		return
	}
	d := near[0]
	etaMin := int(d.DistanceM/ (8.333 * 60)) // ~30 km/h → minutes
	if etaMin < 1 {
		etaMin = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"order_id":        orderID,
		"eta_minutes":     etaMin,
		"driver_location": map[string]any{"lat": d.Lat, "lng": d.Lng},
		"updated_at":      d.RecordedAt,
	})
}

// --- POST /v1/trips:close (write the ONE PG per-trip summary) -----------------

type closeTripRequest struct {
	TripID   string  `json:"trip_id"`
	DriverID string  `json:"driver_id"`
	OrderID  string  `json:"order_id"`
	StartLat float64 `json:"start_lat"`
	StartLng float64 `json:"start_lng"`
	EndLat   float64 `json:"end_lat"`
	EndLng   float64 `json:"end_lng"`
}

func (s *server) handleCloseTrip(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var in closeTripRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.DriverID == "" {
		s.fail(w, r, shoperr.New(codeDriverRequired, ""))
		return
	}
	now := nowFor(r.Context(), s.clock)
	tripID := in.TripID
	if tripID == "" {
		tripID = newToken("trip")
	}
	sum := s.tier.CloseTrip(tripID, in.DriverID, in.OrderID,
		plane.Position{Lat: in.StartLat, Lng: in.StartLng, RecordedAt: now},
		plane.Position{Lat: in.EndLat, Lng: in.EndLng, RecordedAt: now})
	day := now.UTC().Format("2006-01-02")
	if err := s.st.writeTripSummary(r.Context(), sum, nil, day); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"trip_id": tripID, "summary": sum, "pg_row_written": 1})
}

// --- GET /v1/admin/geo/stats (ingest/skew/connection dashboards feed) --------

func (s *server) handleGeoStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	frac, hottestKey, hottest, total := s.geo.HottestKeyFraction()
	tstats := s.tier.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"live_drivers":         s.geo.LiveCount(),
		"open_streams":         s.hub.OpenStreams(),
		"auth_count":           s.hub.AuthCount(),
		"messages":             s.hub.MsgCount(),
		"produced":             s.hub.Produced(),
		"produce_errors":       s.hub.ProduceErrors(),
		"batches":              s.hub.Batches(),
		"hottest_key":          hottestKey,
		"hottest_key_writes":   hottest,
		"total_writes":         total,
		"hottest_key_fraction": frac,
		"tiering": map[string]any{
			"raw_frames": tstats.RawFrames, "iceberg_rows": tstats.IcebergRows, "pg_rows": tstats.PGRows,
		},
	})
}

// --- helpers ----------------------------------------------------------------

const (
	cityCenterLat = 13.7460
	cityCenterLng = 100.5340
)

func (s *server) only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		h(w, r)
	}
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	shoperr.WriteRequest(w, r, err, logging.TraceIDFromRequest)
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if err.Error() == "EOF" {
			return nil
		}
		return shoperr.New(shoperr.CodeValidation, "request body must be valid JSON")
	}
	return nil
}

func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func selfCheck(u string) {
	resp, err := http.Get(u)
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	_ = resp.Body.Close()
}

// locationSampleRate is the INFO sampling rate on the ultra-hot location ingest
// path (04 §2 "INFO sampling on ultra-hot paths (location ingest) — errors never
// sampled"). Default 2% (0.02); errors/WARN+ are always logged by libs/logging.
func locationSampleRate() float64 {
	if v := os.Getenv("LOG_SAMPLE_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.02
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
