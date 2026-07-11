// Command pricing-promo is the V-T8 Pricing & quotes slice (D10; the Growth
// team's quote engine). It prices a cart — items + typed fees[] (delivery,
// service, surge) + typed discounts[] (promos, vouchers) → total, in integer
// minor units + ISO currency (02 §1 / §5) — and returns an HMAC-signed quote
// with a 10-min TTL. The live quote lives in a Redis-like TTL tier; PG
// persistence happens ONLY at checkout (D10).
//
// The four headline correctness properties (proved for real, under -race unless
// noted):
//
//   - DETERMINISTIC PRICING MATH (pricing.go): integer-only fees/discounts/total,
//     surge from an injected clock; byte-identical on reruns (pricing_test.go).
//   - HMAC-SIGNED QUOTES, TAMPER/EXPIRY ⇒ 422 (sign.go/keys.go): a quote is
//     signed over its canonical body + expiry with a ROTATING key; a tampered or
//     expired quote is rejected with 422 on 100% of a fixture sweep — mirroring
//     V-T1's 1000/1000 forgery rigor (sign_test.go).
//   - PG PERSISTENCE ONLY AT CHECKOUT (store.go): POST /v1/quotes writes ZERO PG
//     rows; the checkout path writes exactly one (store_test.go).
//   - QUOTE p99 < 300 ms (perf_test.go, no -race): measured per-quote latency.
//
// Endpoints (02 §1 conventions, 02 §2 error envelope; POST /v1/quotes is gated by
// the `pricing_v1` flag, libs/flags):
//
//	POST /v1/quotes                       price a cart → signed quote (10-min TTL); NO PG write
//	GET  /v1/quotes/{quote_id}            retrieve a live quote from the Redis-like tier (verifies sig+expiry)
//	POST /v1/quotes/{quote_id}:checkout   consume a signed quote at checkout: verify (tamper/expiry ⇒ 422) → persist ONE PG row
//	POST /v1/pricing/keys:rotate          rotation runbook step 1 (admin, non-prod)
//	POST /v1/pricing/keys:retire          rotation runbook step 2 (admin, non-prod)
//
// Sandbox adaptations (disclosed in VERIFICATION.md §V-T8): the "Redis" 10-min
// TTL tier is an in-process quoteCache (no daemon here); the PG store is
// in-memory SQLite in tests (production schema migrations/0001_pricing.pg.sql);
// HMAC keys are generated in-process and rotated via the admin endpoints (a
// production deployment loads seed secrets from the per-cell secret store keyed
// by kid). The signing/verification, rotation, deterministic math, and
// PG-only-at-checkout LOGIC — the correctness of this slice — is real and fully
// tested.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

// Domain error codes owned by this slice. Tamper/expiry are DOMAIN_RULE (422) per
// the V-T8 test criteria (02 §2 maps domain-rule violations to 422).
var (
	codePricingDisabled = shoperr.Register("PRICING_DISABLED", 404, false, "The pricing_v1 feature is not enabled.")
	codeCartNotFound    = shoperr.Register("QUOTE_CART_NOT_FOUND", 404, false, "No cart exists for this id.")
	codeCartUnavailable = shoperr.Register("QUOTE_CART_UNAVAILABLE", 503, true, "The cart service is temporarily unavailable; retry shortly.")
	codeQuoteNotFound   = shoperr.Register("QUOTE_NOT_FOUND", 404, false, "No live quote exists for this id (it may have expired).")
	codeQuoteInvalid    = shoperr.Register("QUOTE_INVALID", 422, false, "The quote failed signature verification (tampered or forged).")
	codeQuoteExpired    = shoperr.Register("QUOTE_EXPIRED", 422, false, "The quote has expired; request a fresh quote.")
	codeAdminDisabled   = shoperr.Register("PRICING_ADMIN_DISABLED", 403, false, "Key-rotation endpoints are disabled in this build.")
)

type server struct {
	cfg     pricingConfig
	km      *keyManager
	cache   *quoteCache
	st      *store
	fetcher cartFetcher
	clock   Clock
	quoteTTL time.Duration
	log     *logging.Logger
	flags   *flags.Set
	enabled bool // pricing_v1 default (per-request override still honoured in non-prod)
	admin   bool // key-rotation endpoints enabled (ENV != prod)
}

func main() {
	port := envOr("PORT", "8107")
	name := envOr("SERVICE_NAME", "pricing-promo")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	cartOrigin := envOr("CART_URL", "http://localhost:8104") // cart slot (V-T7)
	quoteTTL := envDuration("QUOTE_TTL", 10*time.Minute)      // D10: 10-min TTL

	ctx := context.Background()
	clk := SystemClock{}
	km, err := newKeyManager(clk)
	if err != nil {
		log.Fatalf("pricing-promo: keygen: %v", err)
	}
	st, err := openStore(ctx, region, clk)
	if err != nil {
		log.Fatalf("pricing-promo: open store: %v", err)
	}

	fs := flags.FromEnv()
	srv := &server{
		cfg:      pricingConfigFromEnv(),
		km:       km,
		cache:    newQuoteCache(clk, quoteTTL),
		st:       st,
		fetcher:  newHTTPCart(cartOrigin),
		clock:    clk,
		quoteTTL: quoteTTL,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("pricing_v1", false),
		admin:   env != "prod",
	}

	mux := srv.mux()
	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("pricing-promo %q on %s (env=%s region=%s pricing_v1=%v cart=%s quote_ttl=%s primary_kid=%s)",
		name, addr, env, region, srv.enabled, cartOrigin, quoteTTL, km.primaryKID())
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("pricing-promo server exited: %v", err)
	}
}

// mux builds the routing table (kept in sync with the test, which rebuilds the
// same set — as cart/merchant-catalog do).
func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/quotes", s.only(http.MethodPost, s.handleCreateQuote))
	mux.HandleFunc("/v1/quotes/", s.handleQuoteSubtree)
	mux.HandleFunc("/v1/pricing/keys:rotate", s.only(http.MethodPost, s.handleRotate))
	mux.HandleFunc("/v1/pricing/keys:retire", s.only(http.MethodPost, s.handleRetire))
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "pricing-promo",
		"pricing_v1":    s.pricingEnabled(r),
		"primary_kid":   s.km.primaryKID(),
		"key_count":     len(s.km.kids()),
		"otel_exporter": otel.ExporterMode(),
	})
}

// pricingEnabled resolves the pricing_v1 flag for this request: the env default,
// with a per-request X-Flag-Override honoured in non-prod (libs/flags +
// libs/testhooks). Gating POST /v1/quotes on it satisfies "ships dark; e2e runs
// with it on".
func (s *server) pricingEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "pricing_v1", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.pricingEnabled(r) {
		s.fail(w, r, shoperr.New(codePricingDisabled, ""))
		return false
	}
	return true
}

// --- create quote -----------------------------------------------------------

// quoteRequest is the body of POST /v1/quotes. `subtotal` is an OPTIONAL additive
// field (02 §5 additive-only): a BFF that already holds the cart total may pass
// it to skip the cart round-trip; when absent, pricing CONSUMES the cart contract
// (GET /v1/carts/{cart_id}) to read the authoritative subtotal.
type quoteRequest struct {
	CartID           string `json:"cart_id"`
	VoucherCode      string `json:"voucher_code"`
	PromoCode        string `json:"promo_code"` // additive alias
	Subtotal         *money `json:"subtotal"`
	DeliveryLocation *struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"delivery_location"`
}

func (s *server) handleCreateQuote(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var in quoteRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.CartID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "cart_id is required",
			shoperr.Detail{Field: "cart_id", Reason: "required"}))
		return
	}

	// Resolve the cart subtotal + currency: the explicit request subtotal if
	// present, else the cart contract (consumes the cart slot / stub).
	var subtotal int64
	var currency string
	if in.Subtotal != nil {
		if in.Subtotal.Amount < 0 || in.Subtotal.Currency == "" {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "subtotal must be a non-negative amount with a currency"))
			return
		}
		subtotal = in.Subtotal.Amount
		currency = in.Subtotal.Currency
	} else {
		snap, err := s.fetcher.fetchCart(r.Context(), in.CartID)
		if err != nil {
			s.failCart(w, r, err)
			return
		}
		subtotal = snap.Subtotal
		currency = snap.Currency
	}
	if currency == "" {
		currency = "THB"
	}

	code := in.VoucherCode
	if code == "" {
		code = in.PromoCode
	}
	now := nowFor(r.Context(), s.clock)

	fees, discounts, total := computeQuote(s.cfg, quoteInputs{
		cartID:      in.CartID,
		subtotal:    subtotal,
		currency:    currency,
		hasDelivery: in.DeliveryLocation != nil,
		code:        code,
		issuedAt:    now,
	})

	q := &Quote{
		QuoteID:   newToken("qot"),
		CartID:    in.CartID,
		Currency:  currency,
		Subtotal:  money{Amount: subtotal, Currency: currency},
		Fees:      fees,
		Discounts: discounts,
		Total:     total,
		IssuedAt:  now.UTC().Format(time.RFC3339),
		ExpiresAt: now.Add(s.quoteTTL).UTC().Format(time.RFC3339),
	}
	if err := signQuote(s.km, q); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, "sign quote: "+err.Error()))
		return
	}

	// Store in the Redis-like TTL tier ONLY — NO PG write on the quote path (D10).
	if err := s.cache.put(q); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, "cache quote: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, q)
}

// --- quote subtree: GET /{id} and POST /{id}:checkout -----------------------

func (s *server) handleQuoteSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/quotes/")
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "quote id path segment required"))
		return
	}
	if id, ok := strings.CutSuffix(suffix, ":checkout"); ok {
		s.handleCheckout(w, r, id)
		return
	}
	if strings.Contains(suffix, "/") || strings.Contains(suffix, ":") {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown quote path"))
		return
	}
	s.handleGetQuote(w, r, suffix)
}

func (s *server) handleGetQuote(w http.ResponseWriter, r *http.Request, quoteID string) {
	if r.Method != http.MethodGet {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	q, ok := s.cache.get(quoteID)
	if !ok {
		s.fail(w, r, shoperr.New(codeQuoteNotFound, ""))
		return
	}
	// Verify integrity + expiry on read too, so a GET never serves a quote that
	// checkout would reject.
	if err := verifyQuote(s.km, q, nowFor(r.Context(), s.clock)); err != nil {
		s.failVerify(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// checkoutResponse is the body of a successful checkout consume.
type checkoutResponse struct {
	QuoteID   string `json:"quote_id"`
	Status    string `json:"status"` // CHECKED_OUT
	Persisted bool   `json:"persisted"`
	Total     money  `json:"total"`
}

// handleCheckout consumes a signed quote at checkout. The client presents the
// SIGNED quote in the body; the handler verifies the HMAC + expiry (tampered or
// expired ⇒ 422) and, only then, persists exactly ONE PG row (idempotent on
// quote_id). This is the sole PG write in the slice (D10 / property #3).
func (s *server) handleCheckout(w http.ResponseWriter, r *http.Request, pathID string) {
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	var q Quote
	if err := decodeStrict(r, &q); err != nil {
		s.fail(w, r, err)
		return
	}
	// If the body omitted the quote (e.g. only a quote_id was sent), fall back to
	// the live quote from the Redis-like tier.
	if q.Signature == "" {
		cached, ok := s.cache.get(pathID)
		if !ok {
			s.fail(w, r, shoperr.New(codeQuoteNotFound, ""))
			return
		}
		q = *cached
	}
	if q.QuoteID == "" {
		q.QuoteID = pathID
	}
	if pathID != "" && q.QuoteID != pathID {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "quote_id in path does not match body"))
		return
	}

	// THE GATE: verify signature + expiry. Tampered/forged ⇒ 422 QUOTE_INVALID;
	// expired ⇒ 422 QUOTE_EXPIRED.
	if err := verifyQuote(s.km, &q, nowFor(r.Context(), s.clock)); err != nil {
		s.failVerify(w, r, err)
		return
	}

	// Verified — persist the single durable row.
	if err := s.st.persistAtCheckout(r.Context(), &q); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, "persist quote: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, checkoutResponse{
		QuoteID: q.QuoteID, Status: "CHECKED_OUT", Persisted: true, Total: q.Total,
	})
}

// --- key rotation (admin, non-prod) -----------------------------------------

func (s *server) handleRotate(w http.ResponseWriter, r *http.Request) {
	if !s.admin {
		s.fail(w, r, shoperr.New(codeAdminDisabled, ""))
		return
	}
	kid, err := s.km.rotate()
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"primary_kid": kid, "kids": s.km.kids()})
}

func (s *server) handleRetire(w http.ResponseWriter, r *http.Request) {
	if !s.admin {
		s.fail(w, r, shoperr.New(codeAdminDisabled, ""))
		return
	}
	kid, err := s.km.retire()
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retired_kid": kid, "primary_kid": s.km.primaryKID(), "kids": s.km.kids()})
}

// --- error mapping + helpers ------------------------------------------------

func (s *server) failCart(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errCartNotFound):
		s.fail(w, r, shoperr.New(codeCartNotFound, ""))
	case errors.Is(err, errCartUnavailable):
		s.fail(w, r, shoperr.New(codeCartUnavailable, ""))
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
	}
}

func (s *server) failVerify(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errQuoteExpired):
		s.fail(w, r, shoperr.New(codeQuoteExpired, ""))
	case errors.Is(err, errQuoteInvalid):
		s.fail(w, r, shoperr.New(codeQuoteInvalid, ""))
	default:
		s.fail(w, r, shoperr.New(codeQuoteInvalid, ""))
	}
}

// only restricts a handler to one method, else 400 via the error envelope.
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
			return nil // empty body allowed
		}
		return shoperr.New(shoperr.CodeValidation, "request body must be valid JSON")
	}
	return nil
}

// decodeStrict is decode but an empty body is an error (checkout needs a body).
func decodeStrict(r *http.Request, v any) error {
	if r.Body == nil {
		return shoperr.New(shoperr.CodeValidation, "request body required")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// pricingConfigFromEnv builds the deterministic rate table, overridable via env
// (still constant at quote time, so pricing stays deterministic per config).
func pricingConfigFromEnv() pricingConfig {
	cfg := defaultPricingConfig()
	cfg.deliveryBaseMinor = envInt64("PRICING_DELIVERY_BASE_MINOR", cfg.deliveryBaseMinor)
	cfg.serviceFeeBps = envInt64("PRICING_SERVICE_FEE_BPS", cfg.serviceFeeBps)
	cfg.surgeBps = envInt64("PRICING_SURGE_BPS", cfg.surgeBps)
	return cfg
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
