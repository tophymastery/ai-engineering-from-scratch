// Command order is the V-T9 Checkout & order saga slice (D22 CDC outbox/inbox
// exactly-once + D9 transaction-durable idempotency; the Marketplace team's saga
// orchestrator). It owns the order lifecycle state machine (01 §4), drives the
// saga against the published payment/dispatch/pricing contracts + fakes, and is
// the flagship consumer of BOTH the idempotency lib (checkout is effect-once
// under double-tap/retry) and the outbox+inbox CDC path (events are produced and
// consumed exactly-once).
//
// The five headline correctness properties (all proved for real, under -race
// unless noted; adaptations disclosed in VERIFICATION.md §V-T9):
//
//   - FULL STATE MACHINE (states.go): every legal transition + every illegal
//     transition (⇒ 409 ORDER_INVALID_TRANSITION) + every compensation path
//     (payment fail ⇒ cancel; merchant reject ⇒ refund; dispatch exhausted ⇒
//     refund; driver abandon ⇒ re-dispatch), deterministic under an injected
//     clock (states_test.go / saga_test.go).
//   - DURABLE TIMERS survive a crash (timers.go): 1000 pending timers, drop the
//     in-memory sweeper, restart ⇒ 1000/1000 fire within 60s of due, exactly once
//     (timers_test.go).
//   - EXACTLY ONE ORDER EFFECT (store.go/events.go): double Pay tap + BFF retry
//     (same Idempotency-Key, D9) + Kafka redelivery (same event_id, the inbox)
//     all converge to ONE charge / ONE state effect (idempotency_test.go).
//   - AUTO-REMEDIATION (saga.go/timers.go): PAYMENT_PENDING > 15 min ⇒ void +
//     cancel via the durable timer, exactly once, in < 16 min
//     (remediation_test.go).
//   - saga_v1 e2e: checkout → payment → accept → deliver through the BFF
//     (tools/e2e-smoke.sh).
//
// Endpoints (02 §1 conventions, 02 §2 error envelope; mutations require
// Idempotency-Key; POST /v1/orders is gated by the `saga_v1` flag):
//
//	POST /v1/orders                       checkout: create order in PAYMENT_PENDING (idempotent, D9)
//	GET  /v1/orders/{id}                  order detail + current state (ETag)
//	POST /v1/orders/{id}:cancel           cancel (customer/ops); 409 on illegal state
//	POST /v1/orders/{id}:accept           merchant accept (PAID → ACCEPTED)
//	POST /v1/orders/{id}:reject           merchant reject (PAID → CANCELLED [refund])
//	POST /v1/order-events                 inject a payment/dispatch/driver domain event (E2E stub path + inbox)
//	POST /v1/admin/orders:bulk-cancel     bulk compensation (ops console)
//	GET  /v1/admin/orders/stuck           stuck-order console (SLO < 0.05%/day)
//	POST /v1/admin/sweep                  fire due durable timers now (ops)
//
// Sandbox adaptations (disclosed): no Docker/K8s ⇒ process-mode + render-only
// manifests; no live Kafka ⇒ in-memory eventbus + durable SQL inbox (the
// exactly-once path is real); no PG ⇒ in-memory SQLite in tests (production
// schema migrations/0001_order.pg.sql). The state machine, durable-timer fire,
// exactly-once, and remediation LOGIC — the correctness of this slice — is real.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/idempotency"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

type server struct {
	st       *store
	sg       *saga
	consumer *sagaConsumer
	sweeper  *Sweeper
	clock    Clock
	log      *logging.Logger
	flags    *flags.Set
	enabled  bool // saga_v1 default (per-request override honoured in non-prod)
	region   string
	admin    bool
}

func main() {
	port := envOr("PORT", "8105")
	name := envOr("SERVICE_NAME", "order")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "bkk")
	paymentURL := envOr("PAYMENT_URL", "http://localhost:8091") // payment-sim slot (S-T7)

	ctx := context.Background()
	clk := SystemClock{}
	st, err := openStore(ctx, region, clk)
	if err != nil {
		log.Fatalf("order: open store: %v", err)
	}
	pay := PaymentClient(newHTTPPaymentClient(paymentURL))
	sg := newSaga(st, pay, region)
	consumer := newSagaConsumer(sg, st, clk)
	// The sweeper fires due timers by applying their trigger through the saga
	// (compensation + follow-on events included). Production tick 5s (< 60s SLO).
	sweeper := NewSweeper(st, "order-sweeper-"+region, clk, func(ctx context.Context, t TimerRow) error {
		_, _, err := sg.ApplyTrigger(ctx, t.OrderID, t.Trigger, map[string]any{"timer": t.Kind}, clk.Now())
		if err != nil && !isInvalidTransition(err) && !isNotFound(err) {
			return err
		}
		return nil
	})

	fs := flags.FromEnv()
	srv := &server{
		st: st, sg: sg, consumer: consumer, sweeper: sweeper, clock: clk,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("saga_v1", false),
		region:  region,
		admin:   env != "prod",
	}

	// Wire the in-memory eventbus: the CDC relay tails our outbox → broker, and
	// the saga consumer subscribes to payment/dispatch/driver topics. No live
	// Kafka in-sandbox (disclosed); the exactly-once inbox path is real.
	broker := eventbus.NewMemBroker(eventbus.WithPartitions(4))
	for _, topic := range ConsumedTopics {
		if _, err := broker.Subscribe(eventbus.SubscribeConfig{Topic: topic, Group: "order"}, consumer.Handle); err != nil {
			log.Fatalf("order: subscribe %s: %v", topic, err)
		}
	}
	go sweeper.Run(ctx, envDuration("SWEEP_TICK", 5*time.Second))

	mux := srv.mux()
	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("order %q on %s (env=%s region=%s saga_v1=%v payment=%s)",
		name, addr, env, region, srv.enabled, paymentURL)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("order server exited: %v", err)
	}
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/orders", s.only(http.MethodPost, s.handleCheckout))
	mux.HandleFunc("/v1/orders/", s.handleOrderSubtree)
	mux.HandleFunc("/v1/order-events", s.only(http.MethodPost, s.handleInjectEvent))
	mux.HandleFunc("/v1/admin/orders:bulk-cancel", s.only(http.MethodPost, s.handleBulkCancel))
	mux.HandleFunc("/v1/admin/orders/stuck", s.only(http.MethodGet, s.handleStuck))
	mux.HandleFunc("/v1/admin/sweep", s.only(http.MethodPost, s.handleSweep))
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "order",
		"saga_v1":       s.sagaEnabled(r),
		"region":        s.region,
		"otel_exporter": otel.ExporterMode(),
	})
}

// sagaEnabled resolves saga_v1 for this request (env default + non-prod
// X-Flag-Override).
func (s *server) sagaEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "saga_v1", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.sagaEnabled(r) {
		s.fail(w, r, shoperr.New(codeSagaDisabled, ""))
		return false
	}
	return true
}

// --- checkout (POST /v1/orders) ---------------------------------------------

// checkoutRequest is the body of POST /v1/orders (02 §4.1).
type checkoutRequest struct {
	QuoteID         string `json:"quote_id"`
	PaymentMethodID string `json:"payment_method_id"`
	CustomerID      string `json:"customer_id"`
	MerchantID      string `json:"merchant_id"`
	ScheduledAt     string `json:"scheduled_at"` // 02 §5 additive (scheduled orders)
	Total           *money `json:"total"`        // optional explicit total (BFF may pass the quoted total)
}

// orderView is the Order response body (02 §4.1 / order.v1.yaml Order schema).
type orderView struct {
	OrderID   string `json:"order_id"`
	Status    string `json:"status"`
	Total     money  `json:"total"`
	CreatedAt string `json:"created_at"`
}

func toView(o OrderRow) orderView {
	return orderView{OrderID: o.OrderID, Status: string(o.Status), Total: o.Total, CreatedAt: o.CreatedAt.UTC().Format(time.RFC3339)}
}

// handleCheckout is the D9 flagship: an idempotent checkout. The idempotency lib
// runs the create-order effect exactly once for an Idempotency-Key (double tap +
// BFF retry ⇒ one order), atomically committing the order row, the CREATED event,
// the order.created outbox event, and the durable PAYMENT_PENDING remediation
// timer. The payment authorization is requested ONCE, post-commit, only on the
// fresh (non-replayed) path — a crash before it leaves a stuck PAYMENT_PENDING
// that the remediation timer voids+cancels.
func (s *server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	now := nowFor(r.Context(), s.clock)

	// Capture the response so we can read the created order_id back and request
	// the authorization exactly once on a fresh (non-replayed) checkout.
	cw := &captureWriter{ResponseWriter: w, header: http.Header{}}
	s.st.idem.HTTP(cw, r, logging.TraceIDFromRequest, func(ctx context.Context, tx idempotency.Execer, body []byte) (int, []byte, error) {
		var in checkoutRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &in); err != nil {
				return 0, nil, shoperr.New(shoperr.CodeValidation, "request body must be valid JSON")
			}
		}
		if in.QuoteID == "" {
			return 0, nil, shoperr.New(shoperr.CodeValidation, "quote_id is required",
				shoperr.Detail{Field: "quote_id", Reason: "required"})
		}
		total := money{Amount: 42550, Currency: "THB"} // default demo total when the BFF passes none
		if in.Total != nil {
			total = *in.Total
		}
		o := OrderRow{
			OrderID:    newToken("ord"),
			CustomerID: orDefault(in.CustomerID, "usr_anon"),
			MerchantID: orDefault(in.MerchantID, "mer_demo"),
			QuoteID:    in.QuoteID,
			Region:     s.region,
			Status:     StatePaymentPending,
			Total:      total,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := s.st.insertOrderTx(ctx, tx, o); err != nil {
			return 0, nil, err
		}
		// Event store: CREATED → QUOTED → PAYMENT_PENDING telescoped at checkout
		// (the quote already exists from pricing-promo). Recorded so the fold
		// (01 §6) reconstructs the full path.
		if err := s.st.appendEventTx(ctx, tx, o.OrderID, TrigQuote, StateCreated, StateQuoted, map[string]any{"quote_id": in.QuoteID}, now); err != nil {
			return 0, nil, err
		}
		if err := s.st.appendEventTx(ctx, tx, o.OrderID, TrigCheckout, StateQuoted, StatePaymentPending, map[string]any{"payment_method_id": in.PaymentMethodID}, now); err != nil {
			return 0, nil, err
		}
		// order.created outbox event — atomic with the order (D22).
		env, err := buildEnvelope("order.created", o, map[string]any{
			"order_id": o.OrderID, "customer_id": o.CustomerID, "status": string(o.Status),
			"total": map[string]any{"amount": o.Total.Amount, "currency": o.Total.Currency}, "item_count": 1,
		}, now)
		if err != nil {
			return 0, nil, err
		}
		if err := s.st.stageEventTx(ctx, tx, "order.created", env); err != nil {
			return 0, nil, err
		}
		// Durable remediation timer: PAYMENT_PENDING > 15 min ⇒ void + cancel.
		if _, err := s.st.scheduleTimerTx(ctx, tx, o.OrderID, KindRemediation, TrigPaymentTimeout, now.Add(DefaultRemediationWindow)); err != nil {
			return 0, nil, err
		}
		resp, _ := json.Marshal(toView(o))
		return http.StatusCreated, resp, nil
	})

	// Post-commit: request the authorization ONCE on a fresh checkout (not on a
	// replayed double-tap/retry). The Idempotency-Replayed header tells us which.
	if cw.status == http.StatusCreated && cw.header.Get(idempotency.ReplayedHeader) != "true" {
		var v orderView
		if json.Unmarshal(cw.body, &v) == nil && v.OrderID != "" {
			s.authorizeOnce(r.Context(), v.OrderID, v.Total)
		}
	}
}

// authorizeOnce requests the single payment authorization for a freshly-created
// order and records the auth_id. Kept OUT of the idempotent DB tx (never call an
// external PSP inside a transaction); effect-once comes from the fresh-vs-replayed
// gate above. A failure/crash here leaves PAYMENT_PENDING for the remediation
// timer to void+cancel — the safety net.
func (s *server) authorizeOnce(ctx context.Context, orderID string, total money) {
	authID, err := s.sg.pay.Authorize(ctx, orderID, total)
	if err != nil || authID == "" {
		return
	}
	_, _ = s.st.db.ExecContext(ctx, `UPDATE orders SET auth_id = ? WHERE order_id = ?`, authID, orderID)
}

// captureWriter records status/header/body while forwarding writes to the client,
// so the checkout handler can read the created order_id back post-commit.
type captureWriter struct {
	http.ResponseWriter
	header http.Header
	status int
	body   []byte
	wrote  bool
}

func (c *captureWriter) Header() http.Header {
	// Merge: expose our capture map but keep it in sync with the real writer.
	return c.mergedHeader()
}

func (c *captureWriter) mergedHeader() http.Header { return c.ResponseWriter.Header() }

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	for k, vs := range c.ResponseWriter.Header() {
		c.header[k] = vs
	}
	c.ResponseWriter.WriteHeader(code)
	c.wrote = true
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	c.body = append(c.body, b...)
	return c.ResponseWriter.Write(b)
}

// --- order subtree: GET /{id}, :cancel, :accept, :reject --------------------

func (s *server) handleOrderSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/orders/")
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "order id path segment required"))
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":cancel"); ok {
		s.handleAction(w, r, id, TrigUserCancel)
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":accept"); ok {
		s.handleAction(w, r, id, TrigMerchantAccept)
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":reject"); ok {
		s.handleAction(w, r, id, TrigMerchantReject)
		return
	}
	if strings.ContainsAny(suffix, "/:") {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown order path"))
		return
	}
	s.handleGetOrder(w, r, suffix)
}

func (s *server) handleGetOrder(w http.ResponseWriter, r *http.Request, orderID string) {
	if r.Method != http.MethodGet {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	o, ok, err := s.st.getOrder(r.Context(), orderID)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codeOrderNotFound, ""))
		return
	}
	writeJSON(w, http.StatusOK, toView(o))
}

// handleAction applies a saga trigger from a BFF action verb (:cancel/:accept/
// :reject). 409 ORDER_INVALID_TRANSITION on an illegal state (02 §4.1).
func (s *server) handleAction(w http.ResponseWriter, r *http.Request, orderID string, trig Trigger) {
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	now := nowFor(r.Context(), s.clock)
	o, applied, err := s.sg.ApplyTrigger(r.Context(), orderID, trig, map[string]any{"actor": "bff"}, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_ = applied
	writeJSON(w, http.StatusOK, toView(o))
}

// --- domain-event injection (E2E stub path + inbox redelivery) --------------

func (s *server) handleInjectEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var env eventbus.Envelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "request body must be a valid event envelope"))
		return
	}
	if env.EventID == "" {
		env.EventID = newToken("evt")
	}
	if _, err := s.consumer.InjectEnvelope(r.Context(), env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	o, ok, _ := s.st.getOrder(r.Context(), env.Aggregate.ID)
	if !ok {
		writeJSON(w, http.StatusAccepted, map[string]any{"applied": true, "event_type": env.EventType})
		return
	}
	writeJSON(w, http.StatusOK, toView(o))
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

func isNotFound(err error) bool {
	var pe *shoperr.Error
	if asErr(err, &pe) {
		return pe.Code == "ORDER_NOT_FOUND" || pe.Code == "NOT_FOUND"
	}
	return false
}

func asErr(err error, target **shoperr.Error) bool {
	for err != nil {
		if pe, ok := err.(*shoperr.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
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

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
