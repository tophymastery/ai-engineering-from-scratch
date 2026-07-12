// Command payment is the V-T10 Payment authorize/capture/refund slice (D9
// transaction-durable idempotency; the Payments team's money-mutation flagship).
// It authorizes/captures/refunds against a PSP (the payment-sim fake in-sandbox,
// a real acquirer in prod), supports a stored-value wallet, publishes payment.*
// events, consumes order contract stubs (order.delivered ⇒ capture,
// order.cancelled ⇒ void), and confirms PSP webhooks exactly-once.
//
// The headline correctness properties (all proved for real, under -race unless
// noted; adaptations disclosed in VERIFICATION.md §V-T10):
//
//   - D9 PG-DURABLE IDEMPOTENCY ON EVERY MONEY MUTATION (payments.go): authorize,
//     capture and refund each run the money effect (PSP charge + payment row +
//     payment.* outbox event) in the SAME transaction as the UNIQUE(idempotency_key)
//     insert. A retried mutation (same Idempotency-Key) is exactly-once at the DB
//     level — one charge / one capture / one refund (idempotency_test.go).
//   - FORCED REDIS FAILOVER ⇒ ZERO DUPLICATE CHARGES, ZERO LOST ORDERS: under a
//     concurrent authorize storm (retries + double-taps) the in-process Redis-like
//     advisory cache is DROPPED mid-storm; the PG UNIQUE constraint (not the cache)
//     is the source of truth, so the end state is exactly one charge per order and
//     zero lost payments (failover_test.go).
//   - WEBHOOK 10× REPLAY ⇒ SINGLE STATE TRANSITION (webhooks.go): a PSP webhook
//     replayed 10× (same event_id) is deduped by the durable SQL inbox to exactly
//     one confirmation (webhook_test.go).
//   - DECLINE / TIMEOUT FIXTURES (psp.go): card …0002 ⇒ payment.failed (order
//     compensates); card …0044 ⇒ timeout handled by bounded retry + circuit
//     breaker (decline_timeout_test.go).
//   - payment_v1 e2e: authorize → capture → refund via the BFF (tools/e2e-smoke.sh).
//
// Endpoints (02 §1 conventions, 02 §2 error envelope; mutations require
// Idempotency-Key; gated by the `payment_v1` flag):
//
//	POST /v1/payments:authorize            authorize a payment (idempotent, D9)
//	GET  /v1/payments/{id}                 payment detail + current status
//	POST /v1/payments/{id}:capture         capture an authorized payment (idempotent, D9)
//	POST /v1/payments/{id}:refund          refund a captured payment (idempotent, D9)
//	POST /v1/wallet:credit                 top up a wallet (idempotent, D9)
//	GET  /v1/wallet/{customer_id}          wallet balance
//	POST /v1/psp/webhooks                  receive PSP webhooks (inbox-deduped)
//	POST /v1/payment-events                inject an order.* domain event (E2E stub path + inbox)
//	POST /v1/admin/payments/{id}:refund    ops refund console (admin-bff)
//	GET  /v1/admin/payments                payments board (admin-bff)
//
// Sandbox adaptations (disclosed): no Docker/K8s ⇒ process-mode + render-only
// manifests; no live Kafka ⇒ in-memory eventbus + durable SQL inbox (the
// exactly-once path is real); no Redis daemon ⇒ an in-process droppable cache
// (the thing the failover test fails); no PG ⇒ in-memory SQLite in tests
// (production schema migrations/0001_payment.pg.sql). The D9 exactly-once money
// mutation, zero-dup/zero-lost under cache failover, and webhook dedupe LOGIC —
// the correctness of this slice — is real.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/idempotency"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

// codePaymentDisabled is the 404 returned when payment_v1 is off (ships dark).
var codePaymentDisabled = shoperr.Register("PAYMENT_DISABLED", 404, false, "The payment_v1 feature is not enabled.")

type server struct {
	st       *store
	pm       *payments
	webhooks *webhookConsumer
	orders   *orderConsumer
	clock    Clock
	log      *logging.Logger
	flags    *flags.Set
	enabled  bool // payment_v1 default (per-request override honoured in non-prod)
	region   string
	admin    bool
}

func main() {
	port := envOr("PORT", "8106")
	name := envOr("SERVICE_NAME", "payment")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "bkk")
	pspURL := envOr("PAYMENT_SIM_URL", "http://localhost:8091") // payment-sim PSP slot (S-T7)
	// Where the PSP posts async webhooks (this service's own endpoint). In the
	// shared E2E env this is the payment slot's routable address.
	selfURL := envOr("PAYMENT_SELF_URL", "http://localhost:"+port)

	ctx := context.Background()
	clk := SystemClock{}
	st, err := openStore(ctx, region, clk)
	if err != nil {
		log.Fatalf("payment: open store: %v", err)
	}
	psp := newResilientPSP(newHTTPPSP(pspURL), clk)
	pm := newPayments(st, psp, region, selfURL+"/v1/psp/webhooks")
	webhooks := newWebhookConsumer(st, clk)
	orders := newOrderConsumer(pm, st, clk)

	fs := flags.FromEnv()
	srv := &server{
		st: st, pm: pm, webhooks: webhooks, orders: orders, clock: clk,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("payment_v1", false),
		region:  region,
		admin:   env != "prod",
	}

	// Wire the in-memory eventbus: the CDC relay tails our outbox → broker, and the
	// order consumer subscribes to order.* topics. No live Kafka in-sandbox
	// (disclosed); the exactly-once inbox path is real.
	broker := eventbus.NewMemBroker(eventbus.WithPartitions(4))
	for _, topic := range ConsumedOrderTopics {
		if _, err := broker.Subscribe(eventbus.SubscribeConfig{Topic: topic, Group: "payment"}, orders.Handle); err != nil {
			log.Fatalf("payment: subscribe %s: %v", topic, err)
		}
	}

	mux := srv.mux()
	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("payment %q on %s (env=%s region=%s payment_v1=%v psp=%s)",
		name, addr, env, region, srv.enabled, pspURL)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("payment server exited: %v", err)
	}
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/payments:authorize", s.only(http.MethodPost, s.handleAuthorize))
	mux.HandleFunc("/v1/payments/", s.handlePaymentSubtree)
	mux.HandleFunc("/v1/wallet:credit", s.only(http.MethodPost, s.handleWalletCredit))
	mux.HandleFunc("/v1/wallet/", s.only(http.MethodGet, s.handleWalletBalance))
	mux.HandleFunc("/v1/psp/webhooks", s.only(http.MethodPost, s.handleWebhook))
	mux.HandleFunc("/v1/payment-events", s.only(http.MethodPost, s.handleInjectOrderEvent))
	mux.HandleFunc("/v1/admin/payments", s.only(http.MethodGet, s.handleAdminPayments))
	mux.HandleFunc("/v1/admin/payments/", s.handleAdminPaymentSubtree)
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "payment",
		"payment_v1":    s.paymentEnabled(r),
		"region":        s.region,
		"otel_exporter": otel.ExporterMode(),
	})
}

// paymentEnabled resolves payment_v1 for this request (env default + non-prod
// X-Flag-Override).
func (s *server) paymentEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "payment_v1", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.paymentEnabled(r) {
		s.fail(w, r, shoperr.New(codePaymentDisabled, ""))
		return false
	}
	return true
}

// --- authorize (POST /v1/payments:authorize) --------------------------------

// handleAuthorize is the D9 flagship money mutation: an idempotent authorize.
func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	s.st.idem.HTTP(w, r, logging.TraceIDFromRequest, func(ctx context.Context, tx idempotency.Execer, body []byte) (int, []byte, error) {
		return s.pm.Authorize(ctx, tx, body, nowFor(ctx, s.clock))
	})
}

// --- payment subtree: GET /{id}, :capture, :refund --------------------------

func (s *server) handlePaymentSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/payments/"), "/")
	if suffix == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "payment id path segment required"))
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":capture"); ok {
		s.handleCaptureRefund(w, r, id, false)
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":refund"); ok {
		s.handleCaptureRefund(w, r, id, true)
		return
	}
	if strings.ContainsAny(suffix, "/:") {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown payment path"))
		return
	}
	s.handleGetPayment(w, r, suffix)
}

func (s *server) handleGetPayment(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	p, ok, err := s.st.getPayment(r.Context(), id)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codePaymentNotFound, ""))
		return
	}
	writeJSON(w, http.StatusOK, toView(p))
}

// handleCaptureRefund runs a D9-idempotent capture or refund. The payment is
// pre-read (for auth_id/capture_id/amount) OUTSIDE the idempotent tx; the money
// mutation + the UNIQUE(idempotency_key) insert commit atomically inside it.
func (s *server) handleCaptureRefund(w http.ResponseWriter, r *http.Request, id string, refund bool) {
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	pre, ok, err := s.st.getPayment(r.Context(), id)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	if !ok {
		s.fail(w, r, shoperr.New(codePaymentNotFound, ""))
		return
	}
	s.st.idem.HTTP(w, r, logging.TraceIDFromRequest, func(ctx context.Context, tx idempotency.Execer, _ []byte) (int, []byte, error) {
		now := nowFor(ctx, s.clock)
		if refund {
			return s.pm.refundExec(ctx, tx, pre, "api", now)
		}
		return s.pm.captureExec(ctx, tx, pre, "api", now)
	})
}

// --- wallet -----------------------------------------------------------------

func (s *server) handleWalletCredit(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	s.st.idem.HTTP(w, r, logging.TraceIDFromRequest, func(ctx context.Context, tx idempotency.Execer, body []byte) (int, []byte, error) {
		return s.pm.Credit(ctx, tx, body, nowFor(ctx, s.clock))
	})
}

func (s *server) handleWalletBalance(w http.ResponseWriter, r *http.Request) {
	customer := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/wallet/"), "/")
	if customer == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "customer_id path segment required"))
		return
	}
	bal, _, err := s.st.walletBalance(r.Context(), customer)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"customer_id": customer, "balance": bal})
}

// --- order-event injection (E2E stub path + inbox redelivery) ---------------

func (s *server) handleInjectOrderEvent(w http.ResponseWriter, r *http.Request) {
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
	if _, err := s.orders.InjectEnvelope(r.Context(), env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	p, ok, _ := s.st.getPaymentByOrder(r.Context(), env.Aggregate.ID)
	if !ok {
		writeJSON(w, http.StatusAccepted, map[string]any{"applied": true, "event_type": env.EventType})
		return
	}
	writeJSON(w, http.StatusOK, toView(p))
}

// --- admin subtree ----------------------------------------------------------

func (s *server) handleAdminPaymentSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin/payments/"), "/")
	if id, ok := strings.CutSuffix(suffix, ":refund"); ok {
		if r.Method != http.MethodPost {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		s.handleAdminRefund(w, r, id)
		return
	}
	s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown admin payment path"))
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
