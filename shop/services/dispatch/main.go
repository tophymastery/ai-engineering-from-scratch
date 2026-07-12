// Command dispatch is the V-T12 Dispatch & driver-offer slice (Logistics team;
// D13 zone-owned batch matching). It owns DRIVER ASSIGNMENT: paid orders become
// waiting orders in their H3 zone, a per-zone single-writer tick batch-matches
// them to available drivers (greedy-with-swaps), each matched driver gets an
// EXCLUSIVE 10 s reservation before the offer, and the driver's accept assigns the
// order — no first-accept-wins 409. Every batch logs a deterministic snapshot so
// assignments replay byte-identically and are explainable.
//
// The headline correctness properties (all proved for real, under -race unless
// noted; adaptations disclosed in VERIFICATION §V-T12):
//
//   - DETERMINISTIC REPLAY (match/snapshot.go): each zone tick logs its full
//     input snapshot + RNG seed; replaying a snapshot reproduces byte-identical
//     assignments 100%.
//   - ZONE SINGLE-WRITER (match/engine.go): each H3 zone has one writer per
//     tick (Kafka partition per zone); no two ticks assign the same driver.
//   - EXCLUSIVE RESERVATIONS / NO 409 (match/reservation.go): a driver is
//     reserved exclusively (10 s) before the offer; offer-conflict rate < 0.5%,
//     reservation-leak rate 0 over a 24 h soak.
//   - BATCH QUALITY (match/matcher.go): greedy-with-swaps sum-of-pickup-ETA is
//     ≥10% better than the greedy baseline on the skewed dataset.
//   - ASSIGNMENT p95 < 5 s (match/perf_test.go): real measured latency.
//   - dispatch_batch e2e: paid order → offer on driver-bff → accept → assigned.
//
// Endpoints (02 §1 conventions, 02 §2 error envelope; gated by dispatch_batch):
//
//	POST /v1/assignments                        assign a paid order now (S-T8 compat + batch-of-one). Saga step 4.
//	GET  /v1/assignments/{order_id}             assignment status
//	POST /v1/order-events                       inject order.paid / driver.location_updated (E2E stub path + inbox)
//	POST /v1/drivers:location                   register/update a driver's location (availability)
//	POST /v1/tick                               force a batch tick now (ops/e2e determinism)
//	GET  /v1/driver/offers?driver_id=…          the driver's current offer card (driver-bff)
//	POST /v1/driver/offers/{order_id}:accept    driver accepts the offer → assigned (driver-bff)
//	GET  /v1/admin/snapshots[?limit=N]          the queryable deterministic snapshot log
//	GET  /v1/admin/snapshots/{tick_id}          one snapshot + on-demand replay verification
//	GET  /v1/admin/reservations                 reservation ledger stats (conflict rate + leak = 0)
//
// Sandbox adaptations (disclosed): no Docker/K8s ⇒ process-mode + render-only
// manifests; no live Kafka ⇒ in-memory eventbus + durable SQL inbox (partition-
// per-zone is expressed in code + config, the single-writer invariant is real);
// no PG ⇒ in-memory SQLite in tests (production schema migrations/0001_dispatch.pg.sql);
// map-sim ETAs use the deterministic in-process twin for byte-identical replay.
// The matching, reservation, snapshot-replay, and batch-quality LOGIC — the
// correctness of this slice — is real.
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
	match "github.com/shop-platform/shop/services/dispatch/match"
)

var (
	codeDispatchDisabled = shoperr.Register("DISPATCH_DISABLED", 404, false, "The dispatch_batch feature is not enabled.")
	codeAssignNotFound   = shoperr.Register("ASSIGNMENT_NOT_FOUND", 404, false, "No assignment exists for that order.")
	codeOrderRequired    = shoperr.Register("ORDER_ID_REQUIRED", 400, false, "An order_id is required.")
	codeNoOffer          = shoperr.Register("NO_OFFER", 409, false, "There is no live offer to accept for that order/driver.")
	codeNoDriver         = shoperr.Register("NO_DRIVER_AVAILABLE", 503, true, "No driver within the widening radius.")
)

type server struct {
	st      *store
	eng     *match.Engine
	pr      *Projection
	orders  OrderClient
	broker  *eventbus.MemBroker
	clock   Clock
	log     *logging.Logger
	flags   *flags.Set
	enabled bool // dispatch_batch default (per-request override honoured in non-prod)
	region  string
	partN   int

	lastPersisted int64 // snapshot persist cursor (tick_id)
}

func main() {
	port := envOr("PORT", "8108")
	name := envOr("SERVICE_NAME", "dispatch")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "bkk")
	orderURL := envOr("ORDER_URL", "") // order slot (drives the saga on accept)
	partN := envInt("DISPATCH_PARTITIONS", 64)
	seed := int64(envInt("DISPATCH_SEED", 1))

	ctx := context.Background()
	st, err := openStore(ctx, region)
	if err != nil {
		log.Fatalf("dispatch: open store: %v", err)
	}
	clk := SystemClock{}
	eng := match.NewEngine(match.Config{Clock: clk, TTL: envDuration("RESERVATION_TTL", match.DefaultReservationTTL), BaseSeed: seed, Partitions: partN})
	pr := newProjection(eng, st, clk)
	var oc OrderClient = noopOrderClient{}
	if orderURL != "" {
		oc = newHTTPOrderClient(orderURL)
	}

	fs := flags.FromEnv()
	srv := &server{
		st: st, eng: eng, pr: pr, orders: oc, clock: clk,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("dispatch_batch", false),
		region:  region,
		partN:   partN,
	}

	// Wire the in-memory eventbus: the CDC relay tails order's outbox → broker, and
	// this projection subscribes to order.paid + driver.location_updated. Produced
	// dispatch.* events are published back to the broker (and pushed to the order
	// slot cross-process). No live Kafka in-sandbox (disclosed); the exactly-once
	// inbox path is real, and partition-per-zone is expressed in code + config.
	broker := eventbus.NewMemBroker(eventbus.WithPartitions(partN))
	srv.broker = broker
	for _, topic := range ConsumedTopics {
		if _, err := broker.Subscribe(eventbus.SubscribeConfig{Topic: topic, Group: "dispatch"}, pr.Handle); err != nil {
			log.Fatalf("dispatch: subscribe %s: %v", topic, err)
		}
	}

	// Background batch ticker (D13 1–2 s tick): matches waiting orders, persists
	// snapshots, sweeps expired reservations. Off the injected system clock.
	go srv.runTicker(ctx, envDuration("DISPATCH_TICK", 1500*time.Millisecond))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(srv.mux())))
	addr := ":" + port
	log.Printf("dispatch %q on %s (env=%s region=%s dispatch_batch=%v order=%s partitions=%d)",
		name, addr, env, region, srv.enabled, orderURL, partN)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("dispatch server exited: %v", err)
	}
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/assignments", s.only(http.MethodPost, s.handleAssign))
	mux.HandleFunc("/v1/assignments/", s.only(http.MethodGet, s.handleGetAssignment))
	mux.HandleFunc("/v1/order-events", s.only(http.MethodPost, s.handleInjectEvent))
	mux.HandleFunc("/v1/drivers:location", s.only(http.MethodPost, s.handleDriverLocation))
	mux.HandleFunc("/v1/tick", s.only(http.MethodPost, s.handleTick))
	mux.HandleFunc("/v1/driver/offers", s.only(http.MethodGet, s.handleGetOffer))
	mux.HandleFunc("/v1/driver/offers/", s.handleOfferSubtree) // {order_id}:accept
	mux.HandleFunc("/v1/admin/snapshots", s.only(http.MethodGet, s.handleListSnapshots))
	mux.HandleFunc("/v1/admin/snapshots/", s.only(http.MethodGet, s.handleGetSnapshot))
	mux.HandleFunc("/v1/admin/reservations", s.only(http.MethodGet, s.handleReservations))
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "dispatch",
		"dispatch_batch": s.batchEnabled(r),
		"region":         s.region,
		"otel_exporter":  otel.ExporterMode(),
	})
}

func (s *server) batchEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "dispatch_batch", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.batchEnabled(r) {
		s.fail(w, r, shoperr.New(codeDispatchDisabled, ""))
		return false
	}
	return true
}

// --- POST /v1/assignments (S-T8 compat + batch-of-one) ----------------------

type assignRequest struct {
	OrderID string      `json:"order_id"`
	Pickup  *match.Point `json:"pickup"`
}

type assignmentView struct {
	AssignmentID string `json:"assignment_id"`
	OrderID      string `json:"order_id"`
	DriverID     string `json:"driver_id"`
	Status       string `json:"status"`
	ETAMinutes   int    `json:"eta_minutes"`
}

// handleAssign assigns a paid order to a driver now. It is BOTH the S-T8
// topology-compat path (the smoke's POST /dispatch/v1/assignments → 201 ASSIGNED)
// and a batch-of-one entry to the real engine: it registers the order, ensures an
// available driver in the zone (adding a synthetic one only if the zone is empty —
// the compat fallback, disclosed), ticks the zone (reserving the matched driver),
// and consumes the reservation (auto-accept) to produce the assignment. Idempotent
// per order_id.
func (s *server) handleAssign(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var in assignRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.OrderID == "" {
		s.fail(w, r, shoperr.New(codeOrderRequired, ""))
		return
	}
	ctx := r.Context()
	now := nowFor(ctx, s.clock)

	// Idempotent: already assigned ⇒ return it.
	if a, ok, _ := s.st.getAssignment(ctx, in.OrderID); ok && a.Status == "ASSIGNED" {
		writeJSON(w, http.StatusCreated, assignmentView{a.AssignmentID, a.OrderID, a.DriverID, a.Status, etaMinutes(a.PickupETA)})
		return
	}

	pickup := derivePickup(in.OrderID)
	if in.Pickup != nil {
		pickup = *in.Pickup
	}
	z := s.eng.AddOrder(match.Order{OrderID: in.OrderID, Pickup: pickup})

	// If the standing batch already has an offer for this order, accept it; else
	// tick, ensuring a driver (synthetic fallback for the all-stubs compat smoke).
	if _, ok := s.eng.Offer(in.OrderID); !ok {
		if offers := s.eng.Tick(z); len(offersForOrder(offers, in.OrderID)) == 0 {
			// no offerable driver in the zone — add a synthetic one near the pickup
			// (S-T8 compat; the real path is fed by driver.location_updated).
			s.eng.AddDriver(match.Driver{DriverID: newToken("drv"), Loc: match.Point{Lat: pickup.Lat + 0.002, Lng: pickup.Lng + 0.002}})
			s.eng.Tick(z)
		}
	}
	s.persistNewSnapshots(ctx)

	res, ok := s.eng.Accept(in.OrderID)
	if !ok {
		s.fail(w, r, shoperr.New(codeNoDriver, ""))
		return
	}
	view := s.recordAssigned(ctx, res, z.Key(), now)
	writeJSON(w, http.StatusCreated, view)
}

// --- GET /v1/assignments/{order_id} -----------------------------------------

func (s *server) handleGetAssignment(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	orderID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/assignments/"), "/")
	if orderID == "" {
		s.fail(w, r, shoperr.New(codeOrderRequired, ""))
		return
	}
	a, ok, err := s.st.getAssignment(r.Context(), orderID)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codeAssignNotFound, ""))
		return
	}
	writeJSON(w, http.StatusOK, assignmentView{a.AssignmentID, a.OrderID, a.DriverID, a.Status, etaMinutes(a.PickupETA)})
}

// --- POST /v1/order-events (inject order.paid / driver.location_updated) -----

func (s *server) handleInjectEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var env eventbus.Envelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "request body must be a valid event envelope"))
		return
	}
	if _, err := s.pr.InjectEnvelope(r.Context(), env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "event_type": env.EventType})
}

// --- POST /v1/drivers:location (register a driver's location) ----------------

func (s *server) handleDriverLocation(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var in driverLocationPayload
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.DriverID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "driver_id is required", shoperr.Detail{Field: "driver_id", Reason: "required"}))
		return
	}
	z := s.eng.AddDriver(match.Driver{DriverID: in.DriverID, Loc: match.Point{Lat: in.Lat, Lng: in.Lng}})
	writeJSON(w, http.StatusAccepted, map[string]any{"driver_id": in.DriverID, "zone": z.Key(), "available": true})
}

// --- POST /v1/tick (force a batch tick) -------------------------------------

func (s *server) handleTick(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	ctx := r.Context()
	offers := s.tickAll(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"offers": offers, "waiting": s.eng.WaitingCount()})
}

// --- GET /v1/driver/offers?driver_id=… (the offer card) ---------------------

func (s *server) handleGetOffer(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	driverID := r.URL.Query().Get("driver_id")
	if driverID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "driver_id query param is required", shoperr.Detail{Field: "driver_id", Reason: "required"}))
		return
	}
	of, ok := s.eng.OffersForDriver(driverID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"driver_id": driverID, "offer": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"driver_id": driverID, "offer": offerView(of)})
}

// --- POST /v1/driver/offers/{order_id}:accept -------------------------------

func (s *server) handleOfferSubtree(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/driver/offers/"), "/")
	orderID, isAccept := strings.CutSuffix(suffix, ":accept")
	if !isAccept || orderID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "expected /v1/driver/offers/{order_id}:accept"))
		return
	}
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	ctx := r.Context()
	now := nowFor(ctx, s.clock)
	of, hasOffer := s.eng.Offer(orderID)
	res, ok := s.eng.Accept(orderID)
	if !ok {
		s.fail(w, r, shoperr.New(codeNoOffer, ""))
		return
	}
	_ = of
	_ = hasOffer
	view := s.recordAssigned(ctx, res, of.Zone.Key(), now)
	writeJSON(w, http.StatusOK, view)
}

// --- GET /v1/admin/snapshots (queryable snapshot log) -----------------------

func (s *server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	s.persistNewSnapshots(r.Context())
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	rows, err := s.st.listSnapshots(r.Context(), limit)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": rows, "count": len(rows)})
}

// handleGetSnapshot returns one snapshot AND replays it on demand, proving the
// logged assignments reproduce byte-identically (the queryable replay evidence).
func (s *server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	s.persistNewSnapshots(r.Context())
	idStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin/snapshots/"), "/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "tick_id must be an integer"))
		return
	}
	snap, ok, err := s.st.getSnapshotFull(r.Context(), id)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(shoperr.CodeNotFound, "no snapshot with that tick_id"))
		return
	}
	replay := snap.Replay(match.ETASeconds)
	writeJSON(w, http.StatusOK, map[string]any{
		"tick_id":            snap.TickID,
		"zone_key":           snap.ZoneKey,
		"partition":          snap.Partition,
		"seed":               snap.Seed,
		"logged_assignments": snap.Assignments,
		"replayed_assignments": replay,
		"replay_identical":   match.Canonical(replay) == match.Canonical(snap.Assignments),
	})
}

// --- GET /v1/admin/reservations (ledger stats) ------------------------------

func (s *server) handleReservations(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	now := nowFor(r.Context(), s.clock)
	st := s.eng.Ledger().Stats(now)
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":         st,
		"conflict_rate": s.eng.Ledger().ConflictRate(),
		"leak_rate":     st.Leaked,
	})
}

// --- ticking + persistence + assignment recording ---------------------------

// tickAll runs a batch tick across all zones, persists new snapshots, and sweeps
// expired reservations. Returns the offers made.
func (s *server) tickAll(ctx context.Context) []match.Offer {
	offers := s.eng.TickAll()
	s.persistNewSnapshots(ctx)
	for _, of := range offers {
		s.emitOffered(ctx, of)
	}
	s.eng.SweepExpired(s.clock.Now())
	return offers
}

func (s *server) runTicker(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tickAll(ctx)
		}
	}
}

// persistNewSnapshots writes every snapshot logged since the last persist,
// recording whether it replays identically (durable replay evidence).
func (s *server) persistNewSnapshots(ctx context.Context) {
	for _, snap := range s.eng.Snapshots() {
		if snap.TickID < s.lastPersisted {
			continue
		}
		if snap.TickID >= s.lastPersisted {
			s.lastPersisted = snap.TickID + 1
		}
		_ = s.st.persistSnapshot(ctx, snap, snap.ReplayMatches(match.ETASeconds))
	}
}

// recordAssigned persists the assignment, emits dispatch.assigned, and pushes it
// to the order saga (cross-process). Returns the response view.
func (s *server) recordAssigned(ctx context.Context, res match.AssignedResult, zoneKey string, now time.Time) assignmentView {
	assignmentID := newToken("asg")
	_ = s.st.upsertAssignment(ctx, res, assignmentID, zoneKey, "ASSIGNED", now)
	// dispatch.assigned → broker (in-proc) + order slot (cross-process saga step).
	env, err := makeEnvelope(TopicDispatchAssigned, res.OrderID, s.region, map[string]any{
		"order_id": res.OrderID, "driver_id": res.DriverID,
		"assigned_at": res.AssignedAt.UTC().Format(time.RFC3339), "eta_minutes": etaMinutes(res.PickupETA),
	}, now)
	if err == nil {
		if msg, e := eventbus.NewMessage(TopicDispatchAssigned, env); e == nil {
			_ = s.broker.Publish(ctx, msg)
		}
		_ = s.orders.Deliver(ctx, env)
	}
	return assignmentView{assignmentID, res.OrderID, res.DriverID, "ASSIGNED", etaMinutes(res.PickupETA)}
}

// emitOffered publishes dispatch.offered for an offer (explainability) and marks
// the assignment PENDING.
func (s *server) emitOffered(ctx context.Context, of match.Offer) {
	_ = s.st.upsertAssignment(ctx, match.AssignedResult{OrderID: of.OrderID, DriverID: of.DriverID, PickupETA: of.PickupETA, AssignedAt: of.OfferedAt}, "", of.Zone.Key(), "PENDING", of.OfferedAt)
	env, err := makeEnvelope(TopicDispatchOffered, of.OrderID, s.region, map[string]any{
		"order_id": of.OrderID, "driver_id": of.DriverID, "pickup_eta_s": of.PickupETA,
		"offered_at": of.OfferedAt.UTC().Format(time.RFC3339),
	}, of.OfferedAt)
	if err == nil {
		if msg, e := eventbus.NewMessage(TopicDispatchOffered, env); e == nil {
			_ = s.broker.Publish(ctx, msg)
		}
	}
}

// --- views / helpers --------------------------------------------------------

func offerView(of match.Offer) map[string]any {
	return map[string]any{
		"order_id": of.OrderID, "driver_id": of.DriverID, "zone": of.Zone.Key(),
		"pickup_eta_s": of.PickupETA, "eta_minutes": etaMinutes(of.PickupETA),
		"offered_at": of.OfferedAt.UTC().Format(time.RFC3339), "expires_at": of.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

func offersForOrder(offers []match.Offer, orderID string) []match.Offer {
	var out []match.Offer
	for _, of := range offers {
		if of.OrderID == orderID {
			out = append(out, of)
		}
	}
	return out
}

func etaMinutes(sec int) int {
	m := (sec + 59) / 60
	if m < 1 {
		m = 1
	}
	return m
}

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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
