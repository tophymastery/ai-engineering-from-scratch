// Command merchant-queue is the V-T11 Merchant accept & order-queue slice
// (Marketplace team; D7 CQRS read model + kitchen-capacity admission tokens, D11
// sharding by merchant_id). It owns the MERCHANT INCOMING-ORDER read model — a
// per-merchant queue projected exactly-once from order.* events (sharded by
// merchant_id) — and the accept/reject surface that drives the order saga
// forward, gated by a kitchen-capacity admission control that inflates the quoted
// prep ETA + shows a busy badge INSTEAD of failing checkout.
//
// The headline correctness properties (all proved for real, under -race unless
// noted; adaptations disclosed in VERIFICATION §V-T11):
//
//   - CQRS PROJECTION (projection.go/store.go): order.created/paid/accepted/…
//     projected onto the incoming-order read model, exactly-once via the durable
//     inbox, LWW forward-only ordering, sharded by merchant_id (D11).
//   - PROJECTION PARITY (store.go): after replaying 10k orders' events (shuffled +
//     duplicated), the read model equals an independent reference fold — 100%.
//   - QUEUE FRESHNESS (projection.go): real measured order.paid→visible p99.
//   - ADMISSION TOKENS (admission.go): 30 accepts/10 min (merchant-tunable); a 50×
//     flash-sale ⇒ zero checkout 5xx, accept rate = capacity ± 5%, busy badge +
//     inflated ETA instead of failure.
//   - REBUILD (store.go): rebuild the read model (or one cell) from the event log;
//     the rebuilt model equals the live one (100% parity).
//   - accept → saga proceeds (orderclient.go): an admitted accept calls the order
//     service :accept (order.accepted) so the saga advances.
//
// Endpoints (02 §1 conventions, 02 §2 error envelope; gated by merchant_queue_v1):
//
//	GET  /v1/merchant/orders?merchant_id=…&state=PENDING   the incoming-order queue
//	GET  /v1/merchant/orders/{order_id}                    one queue entry
//	POST /v1/merchant/orders/{order_id}:accept             admit + drive saga (busy⇒defer, not 5xx)
//	POST /v1/merchant/orders/{order_id}:reject             reject (order.cancelled)
//	GET  /v1/merchant/{merchant_id}/capacity               busy badge + prep ETA
//	PUT  /v1/merchant/{merchant_id}/capacity               tune kitchen capacity
//	POST /v1/order-events                                  inject an order.* event (E2E stub path + inbox)
//	POST /v1/admin/rebuild[?cell=N]                        rebuild the read model from the log + parity
//	GET  /v1/admin/freshness                               projection-lag stats (dashboard)
//
// Sandbox adaptations (disclosed): no Docker/K8s ⇒ process-mode + render-only
// manifests; no live Kafka ⇒ in-memory eventbus + durable SQL inbox (the
// exactly-once path is real); no PG ⇒ in-memory SQLite in tests (production
// schema migrations/0001_merchant_queue.pg.sql). The projection, LWW, admission
// arithmetic, and rebuild LOGIC — the correctness of this slice — is real.
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
)

var (
	codeQueueDisabled  = shoperr.Register("MERCHANT_QUEUE_DISABLED", 404, false, "The merchant_queue_v1 feature is not enabled.")
	codeQueueNotFound  = shoperr.Register("QUEUE_ORDER_NOT_FOUND", 404, false, "No incoming order with that id is in the queue.")
	codeNotPending     = shoperr.Register("QUEUE_ORDER_NOT_PENDING", 409, false, "The order is not awaiting merchant acceptance.")
	codeMerchantNeeded = shoperr.Register("MERCHANT_ID_REQUIRED", 400, false, "A merchant_id is required (query param or known from the order).")
	codeOrderUpstream  = shoperr.Register("ORDER_UPSTREAM_UNAVAILABLE", 502, true, "The order service could not be reached to advance the saga.")
	codeAcceptConflict = shoperr.Register("ORDER_INVALID_TRANSITION", 409, false, "The order is not in a state that allows this transition.")
)

type server struct {
	st      *store
	pr      *Projection
	adm     *Admission
	orders  OrderClient
	clock   Clock
	log     *logging.Logger
	flags   *flags.Set
	enabled bool // merchant_queue_v1 default (per-request override honoured in non-prod)
	region  string
}

func main() {
	port := envOr("PORT", "8117")
	name := envOr("SERVICE_NAME", "merchant-queue")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	rebuildDemo := flag.Bool("rebuild-demo", false, "seed N orders, rebuild the largest cell from the log, assert parity, print timing, exit")
	demoN := flag.Int("n", 10000, "order count for -rebuild-demo")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}
	if *rebuildDemo {
		runRebuildDemo(*demoN)
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "bkk")
	orderURL := envOr("ORDER_URL", "") // order slot (drives the saga on accept)

	ctx := context.Background()
	st, err := openStore(ctx, region)
	if err != nil {
		log.Fatalf("merchant-queue: open store: %v", err)
	}
	clk := SystemClock{}
	pr := newProjection(st, clk)
	var oc OrderClient = noopOrderClient{}
	if orderURL != "" {
		oc = newHTTPOrderClient(orderURL)
	}

	fs := flags.FromEnv()
	srv := &server{
		st: st, pr: pr, adm: newAdmission(), orders: oc, clock: clk,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("merchant_queue_v1", false),
		region:  region,
	}

	// Wire the in-memory eventbus: the CDC relay tails order's outbox → broker,
	// and this projection subscribes to the order.* topics. No live Kafka
	// in-sandbox (disclosed); the exactly-once inbox path is real. In the E2E env
	// stub events also arrive via POST /v1/order-events (cross-process delivery).
	broker := eventbus.NewMemBroker(eventbus.WithPartitions(4))
	for _, topic := range ConsumedTopics {
		if _, err := broker.Subscribe(eventbus.SubscribeConfig{Topic: topic, Group: "merchant-queue"}, pr.Handle); err != nil {
			log.Fatalf("merchant-queue: subscribe %s: %v", topic, err)
		}
	}

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(srv.mux())))
	addr := ":" + port
	log.Printf("merchant-queue %q on %s (env=%s region=%s merchant_queue_v1=%v order=%s)",
		name, addr, env, region, srv.enabled, orderURL)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("merchant-queue server exited: %v", err)
	}
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/merchant/orders", s.only(http.MethodGet, s.handleListQueue))
	mux.HandleFunc("/v1/merchant/orders/", s.handleOrderSubtree)
	mux.HandleFunc("/v1/merchant/", s.handleMerchantSubtree) // {merchant_id}/capacity
	mux.HandleFunc("/v1/order-events", s.only(http.MethodPost, s.handleInjectEvent))
	mux.HandleFunc("/v1/admin/rebuild", s.only(http.MethodPost, s.handleRebuild))
	mux.HandleFunc("/v1/admin/freshness", s.only(http.MethodGet, s.handleFreshness))
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "merchant-queue",
		"merchant_queue_v1": s.queueEnabled(r),
		"region":            s.region,
		"otel_exporter":     otel.ExporterMode(),
	})
}

func (s *server) queueEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "merchant_queue_v1", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.queueEnabled(r) {
		s.fail(w, r, shoperr.New(codeQueueDisabled, ""))
		return false
	}
	return true
}

// --- queue read model -------------------------------------------------------

func (s *server) handleListQueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	merchantID := r.URL.Query().Get("merchant_id")
	if merchantID == "" {
		s.fail(w, r, shoperr.New(codeMerchantNeeded, ""))
		return
	}
	state := strings.ToUpper(r.URL.Query().Get("state"))
	if state == "" {
		state = StatePending // the incoming queue = orders awaiting accept
	}
	if state == "ALL" {
		state = ""
	}
	rows, err := s.st.listQueue(r.Context(), merchantID, state)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	pending, _ := s.st.pendingCount(r.Context(), merchantID)
	status := s.adm.Status(merchantID, nowFor(r.Context(), s.clock), pending)
	writeJSON(w, http.StatusOK, map[string]any{
		"merchant_id": merchantID,
		"count":       len(rows),
		"orders":      rows,
		"capacity":    status,
	})
}

func (s *server) handleOrderSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/merchant/orders/"), "/")
	if suffix == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "order id path segment required"))
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":accept"); ok {
		s.handleAccept(w, r, id)
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":reject"); ok {
		s.handleReject(w, r, id)
		return
	}
	if strings.ContainsAny(suffix, "/:") {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown queue path"))
		return
	}
	s.handleGetOrder(w, r, suffix)
}

func (s *server) handleGetOrder(w http.ResponseWriter, r *http.Request, orderID string) {
	if r.Method != http.MethodGet {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	row, ok, err := s.st.getRow(r.Context(), orderID)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codeQueueNotFound, ""))
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// --- accept / reject (drive the saga, admission-gated) ----------------------

type actionBody struct {
	MerchantID string `json:"merchant_id"`
}

// handleAccept: admit against kitchen capacity, then drive the order saga. When
// the kitchen is at capacity the accept is DEFERRED with a busy badge + inflated
// ETA (HTTP 200 — NOT a 5xx): checkout never fails, the order stays PENDING.
func (s *server) handleAccept(w http.ResponseWriter, r *http.Request, orderID string) {
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	row, ok, err := s.st.getRow(r.Context(), orderID)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codeQueueNotFound, ""))
		return
	}
	merchantID := row.MerchantID
	if merchantID == "" {
		var b actionBody
		_ = decode(r, &b)
		merchantID = b.MerchantID
	}
	if merchantID == "" {
		s.fail(w, r, shoperr.New(codeMerchantNeeded, ""))
		return
	}
	if row.QueueState == StateAccepted {
		// Idempotent: already accepted.
		s.respondAck(w, r, orderID, merchantID, StateAccepted, false)
		return
	}
	if row.QueueState != StatePending {
		s.fail(w, r, shoperr.New(codeNotPending, ""))
		return
	}

	now := nowFor(r.Context(), s.clock)
	if !s.adm.TryAccept(merchantID, now) {
		// Kitchen at capacity — DEFER with a busy badge + inflated ETA (not a 5xx).
		pending, _ := s.st.pendingCount(r.Context(), merchantID)
		st := s.adm.Status(merchantID, now, pending)
		writeJSON(w, http.StatusOK, map[string]any{
			"order_id":         orderID,
			"status":           StatePending,
			"busy":             true,
			"deferred":         true,
			"prep_eta_minutes": st.PrepETAMinutes,
			"message":          "kitchen at capacity — accept deferred; ETA inflated, checkout unaffected",
		})
		return
	}

	switch s.orders.Accept(r.Context(), orderID) {
	case acceptOK:
		// Drive the local read model to ACCEPTED via a synthesized order.accepted
		// (goes through the inbox+log so a rebuild reproduces it; the real
		// order.accepted off the bus later dedupes by phase, a no-op).
		env, _ := makeOrderEnvelope("evt_accept_"+orderID, TopicOrderAccepted, orderID, merchantID, s.region,
			map[string]any{"accepted_at": now.UTC().Format(time.RFC3339)}, now)
		_, _ = s.pr.InjectEnvelope(r.Context(), env)
		s.respondAck(w, r, orderID, merchantID, StateAccepted, false)
	case acceptConflict:
		s.adm.Refund(merchantID) // order wasn't acceptable — don't burn a token
		s.fail(w, r, shoperr.New(codeAcceptConflict, ""))
	default: // acceptUpstream
		s.adm.Refund(merchantID)
		s.fail(w, r, shoperr.New(codeOrderUpstream, ""))
	}
}

func (s *server) handleReject(w http.ResponseWriter, r *http.Request, orderID string) {
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	row, ok, err := s.st.getRow(r.Context(), orderID)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codeQueueNotFound, ""))
		return
	}
	if row.QueueState != StatePending && row.QueueState != StateAccepted {
		s.fail(w, r, shoperr.New(codeNotPending, ""))
		return
	}
	now := nowFor(r.Context(), s.clock)
	switch s.orders.Reject(r.Context(), orderID) {
	case acceptOK:
		// A reject cancels the order (order.cancelled with refund). The queue row
		// becomes CANCELLED; the merchant-facing ack reports REJECTED.
		// order.cancelled payload carries only its declared fields (order_id via
		// the aggregate + cancelled_at + reason); the queue row already knows the
		// merchant, so no merchant_id is needed on this event.
		env, _ := makeOrderEnvelope("evt_reject_"+orderID, TopicOrderCancelled, orderID, "", s.region,
			map[string]any{"cancelled_at": now.UTC().Format(time.RFC3339), "reason": "merchant_reject"}, now)
		_, _ = s.pr.InjectEnvelope(r.Context(), env)
		writeJSON(w, http.StatusOK, map[string]any{"order_id": orderID, "status": StateRejected})
	case acceptConflict:
		s.fail(w, r, shoperr.New(codeAcceptConflict, ""))
	default:
		s.fail(w, r, shoperr.New(codeOrderUpstream, ""))
	}
}

func (s *server) respondAck(w http.ResponseWriter, r *http.Request, orderID, merchantID, status string, busy bool) {
	pending, _ := s.st.pendingCount(r.Context(), merchantID)
	st := s.adm.Status(merchantID, nowFor(r.Context(), s.clock), pending)
	writeJSON(w, http.StatusOK, map[string]any{
		"order_id":         orderID,
		"status":           status,
		"busy":             st.Busy || busy,
		"prep_eta_minutes": st.PrepETAMinutes,
	})
}

// --- merchant capacity (busy badge + tuning) --------------------------------

func (s *server) handleMerchantSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/merchant/"), "/")
	mid, rest, found := strings.Cut(suffix, "/")
	if !found || rest != "capacity" || mid == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown merchant path"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		pending, _ := s.st.pendingCount(r.Context(), mid)
		writeJSON(w, http.StatusOK, s.adm.Status(mid, nowFor(r.Context(), s.clock), pending))
	case http.MethodPut:
		var body struct {
			AcceptsPerWindow int `json:"accepts_per_window"`
			WindowMinutes    int `json:"window_minutes"`
		}
		if err := decode(r, &body); err != nil {
			s.fail(w, r, err)
			return
		}
		if body.AcceptsPerWindow <= 0 {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "accepts_per_window must be > 0",
				shoperr.Detail{Field: "accepts_per_window", Reason: "must_be_positive"}))
			return
		}
		win := DefaultWindow
		if body.WindowMinutes > 0 {
			win = time.Duration(body.WindowMinutes) * time.Minute
		}
		s.adm.SetCapacity(mid, body.AcceptsPerWindow, win)
		pending, _ := s.st.pendingCount(r.Context(), mid)
		writeJSON(w, http.StatusOK, s.adm.Status(mid, nowFor(r.Context(), s.clock), pending))
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
	}
}

// --- order-event injection (E2E stub path + inbox redelivery) ---------------

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
	orderID := env.Aggregate.ID
	row, ok, _ := s.st.getRow(r.Context(), orderID)
	if !ok {
		writeJSON(w, http.StatusAccepted, map[string]any{"applied": true, "event_type": env.EventType})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"applied": true, "event_type": env.EventType,
		"order_id": row.OrderID, "queue_state": row.QueueState,
	})
}

// --- rebuild + freshness (ops / dashboard) ----------------------------------

func (s *server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	cell := -1
	if c := r.URL.Query().Get("cell"); c != "" {
		if v, err := strconv.Atoi(c); err == nil {
			cell = v
		}
	}
	res, err := s.st.Rebuild(r.Context(), cell, s.clock.Now())
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleFreshness(w http.ResponseWriter, r *http.Request) {
	n, p50, p99 := s.pr.fresh.stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"samples":  n,
		"p50_ms":   float64(p50.Microseconds()) / 1000.0,
		"p99_ms":   float64(p99.Microseconds()) / 1000.0,
		"budget_ms": 2000,
	})
}

// --- helpers ----------------------------------------------------------------

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
