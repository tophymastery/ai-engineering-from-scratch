package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
)

// --- test harness -----------------------------------------------------------

// fakeCart is the injected cartFetcher: an in-test stand-in for the cart slot's
// GET /v1/carts/{id} read (the pricing-promo→cart pact). Records call counts so a
// test can prove pricing only calls cart when no explicit subtotal is supplied.
type fakeCart struct {
	mu    sync.Mutex
	carts map[string]cartSnapshot
	calls int
	fail  error
}

func newFakeCart() *fakeCart { return &fakeCart{carts: map[string]cartSnapshot{}} }

func (f *fakeCart) seed(cartID string, subtotal int64, currency string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.carts[cartID] = cartSnapshot{CartID: cartID, Subtotal: subtotal, Currency: currency}
}

func (f *fakeCart) fetchCart(_ context.Context, cartID string) (cartSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return cartSnapshot{}, f.fail
	}
	c, ok := f.carts[cartID]
	if !ok {
		return cartSnapshot{}, errCartNotFound
	}
	return c, nil
}

// newTestServer builds an in-process pricing-promo server on in-memory SQLite
// with a ManualClock, an injected fakeCart, and pricing_v1 forced on (the
// e2e/prod default is OFF; tests exercise the enabled path). No Docker, no
// external DB, no Redis — the signing/verification, the Redis-like TTL tier, the
// deterministic engine, and the PG-only-at-checkout write are the real code
// paths. t0 pins the clock OUTSIDE any surge window (09:00 UTC) unless a test
// advances it.
func newTestServer(t *testing.T) (*server, *ManualClock, *fakeCart) {
	t.Helper()
	return newTestServerAt(t, time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC))
}

func newTestServerAt(t *testing.T, t0 time.Time) (*server, *ManualClock, *fakeCart) {
	t.Helper()
	ctx := context.Background()
	clk := NewManualClock(t0)
	km, err := newKeyManager(clk)
	if err != nil {
		t.Fatalf("newKeyManager: %v", err)
	}
	st, err := openStore(ctx, "bkk", clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(st.close)
	fc := newFakeCart()
	srv := &server{
		cfg:      defaultPricingConfig(),
		km:       km,
		cache:    newQuoteCache(clk, 10*time.Minute),
		st:       st,
		fetcher:  fc,
		clock:    clk,
		quoteTTL: 10 * time.Minute,
		log:      logging.New(logging.Config{Service: "pricing-promo", Version: "test", Env: "test", Region: "bkk", SampleRate: 0, Out: &bytes.Buffer{}}),
		flags:    flags.NewSet(map[string]string{"pricing_v1": "true"}),
		enabled:  true,
		admin:    true,
	}
	return srv, clk, fc
}

// do issues a request against the server mux and returns (status, body-map).
func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// doQuote issues a request and unmarshals the body into a Quote.
func doQuote(t *testing.T, h http.Handler, method, path, body string) (int, Quote) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var q Quote
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	return rec.Code, q
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

const (
	tCart = "crt_01test0000000000000000pric"
)

// createBody builds a POST /v1/quotes body with an explicit subtotal (cart-
// independent) plus optional voucher + delivery.
func createBody(cartID string, subtotal int64, currency, voucher string, delivery bool) string {
	req := map[string]any{"cart_id": cartID, "subtotal": map[string]any{"amount": subtotal, "currency": currency}}
	if voucher != "" {
		req["voucher_code"] = voucher
	}
	if delivery {
		req["delivery_location"] = map[string]any{"lat": 13.7563, "lng": 100.5018}
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// --- tests ------------------------------------------------------------------

// TestCreateQuote_TypedLines exercises the core: a quote with typed fees[]
// (DELIVERY + SERVICE) and typed discounts[] (VOUCHER), an integer total, a
// signature + kid, and a 10-min expiry — all in the cart currency.
func TestCreateQuote_TypedLines(t *testing.T) {
	s, clk, _ := newTestServer(t)
	h := s.mux()
	code, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "LUNCH25", true))
	if code != 201 {
		t.Fatalf("create quote: %d", code)
	}
	if q.QuoteID == "" || !strings.HasPrefix(q.QuoteID, "qot_") {
		t.Fatalf("bad quote_id %q", q.QuoteID)
	}
	if q.Subtotal.Amount != 40000 || q.Currency != "THB" {
		t.Fatalf("subtotal %+v currency %q", q.Subtotal, q.Currency)
	}
	// fees: DELIVERY 1900 + SERVICE 10% of 40000 = 4000. off-peak (09:00) → no SURGE.
	assertLine(t, q.Fees, "DELIVERY", 1900)
	assertLine(t, q.Fees, "SERVICE", 4000)
	if hasType(q.Fees, "SURGE") {
		t.Fatalf("unexpected SURGE off-peak: %+v", q.Fees)
	}
	// discounts: VOUCHER LUNCH25 fixed -2500.
	assertLine(t, q.Discounts, "VOUCHER", -2500)
	// total = 40000 + 1900 + 4000 - 2500 = 43400.
	if q.Total.Amount != 43400 || q.Total.Currency != "THB" {
		t.Fatalf("total %+v want 43400 THB", q.Total)
	}
	if q.Kid == "" || q.Signature == "" {
		t.Fatalf("quote not signed: kid=%q sig=%q", q.Kid, q.Signature)
	}
	// expiry = issued + 10 min.
	exp, _ := time.Parse(time.RFC3339, q.ExpiresAt)
	if !exp.Equal(clk.Now().Add(10 * time.Minute).Truncate(time.Second)) && exp.Sub(clk.Now()) != 10*time.Minute {
		t.Fatalf("expiry %v not issued+10m (now=%v)", exp, clk.Now())
	}
}

// TestCreateQuote_ConsumesCartContract proves that WITHOUT an explicit subtotal
// pricing CONSUMES the cart contract (fetchCart) for the authoritative subtotal.
func TestCreateQuote_ConsumesCartContract(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	fc.seed(tCart, 25000, "THB")
	body := `{"cart_id":"` + tCart + `","delivery_location":{"lat":13.7,"lng":100.5}}`
	code, q := doQuote(t, h, "POST", "/v1/quotes", body)
	if code != 201 {
		t.Fatalf("create quote: %d", code)
	}
	if fc.calls != 1 {
		t.Fatalf("expected exactly 1 cart fetch, got %d", fc.calls)
	}
	if q.Subtotal.Amount != 25000 {
		t.Fatalf("subtotal %d want 25000 (from cart)", q.Subtotal.Amount)
	}
	// 25000 + 1900 delivery + 2500 service = 29400.
	if q.Total.Amount != 29400 {
		t.Fatalf("total %d want 29400", q.Total.Amount)
	}
}

// TestCreateQuote_CartNotFound maps a missing cart to 404.
func TestCreateQuote_CartNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	code, m := do(t, h, "POST", "/v1/quotes", `{"cart_id":"crt_missing"}`)
	if code != 404 || errCode(m) != "QUOTE_CART_NOT_FOUND" {
		t.Fatalf("missing cart -> %d %s (want 404 QUOTE_CART_NOT_FOUND)", code, errCode(m))
	}
}

// TestFlagGate: with pricing_v1 off, POST /v1/quotes is disabled (404
// PRICING_DISABLED). Ships dark.
func TestFlagGate(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.flags = flags.NewSet(map[string]string{"pricing_v1": "false"})
	s.enabled = false
	h := s.mux()
	code, m := do(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "", true))
	if code != 404 || errCode(m) != "PRICING_DISABLED" {
		t.Fatalf("flag off -> %d %s (want 404 PRICING_DISABLED)", code, errCode(m))
	}
}

// TestGetQuote retrieves a live quote from the Redis-like tier.
func TestGetQuote(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "LUNCH25", true))
	code, got := doQuote(t, h, "GET", "/v1/quotes/"+q.QuoteID, "")
	if code != 200 || got.QuoteID != q.QuoteID || got.Total.Amount != q.Total.Amount {
		t.Fatalf("get quote -> %d %+v", code, got)
	}
	// unknown id → 404.
	code, m := do(t, h, "GET", "/v1/quotes/qot_nope", "")
	if code != 404 || errCode(m) != "QUOTE_NOT_FOUND" {
		t.Fatalf("get unknown -> %d %s", code, errCode(m))
	}
}

// TestCheckoutHappyPath: a clean signed quote → 200 CHECKED_OUT + persisted.
func TestCheckoutHappyPath(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "LUNCH25", true))
	qb, _ := json.Marshal(q)
	code, m := do(t, h, "POST", "/v1/quotes/"+q.QuoteID+":checkout", string(qb))
	if code != 200 {
		t.Fatalf("checkout -> %d %v", code, m)
	}
	if m["status"] != "CHECKED_OUT" || m["persisted"] != true {
		t.Fatalf("checkout body %v", m)
	}
	// idempotent: a repeat checkout does not create a second row.
	do(t, h, "POST", "/v1/quotes/"+q.QuoteID+":checkout", string(qb))
	n, _ := s.st.quoteRowCount(context.Background())
	if n != 1 {
		t.Fatalf("checkout row count %d want 1 (idempotent)", n)
	}
}

func assertLine(t *testing.T, ls []lineItem, typ string, amount int64) {
	t.Helper()
	for _, l := range ls {
		if l.Type == typ {
			if l.Amount != amount {
				t.Fatalf("line %s amount %d want %d", typ, l.Amount, amount)
			}
			return
		}
	}
	t.Fatalf("line %s not found in %+v", typ, ls)
}

func hasType(ls []lineItem, typ string) bool {
	for _, l := range ls {
		if l.Type == typ {
			return true
		}
	}
	return false
}
