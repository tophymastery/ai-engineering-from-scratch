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

// fakeCatalog is the injected catalogFetcher: an in-test stand-in for the
// merchant-catalog GET /menu read (the pact interaction). It records call counts
// so a test can prove cart only fetches when its view is cold.
type fakeCatalog struct {
	mu    sync.Mutex
	menus map[string]fakeMenu
	calls int
}

type fakeMenu struct {
	version int64
	items   map[string]itemInfo
}

func newFakeCatalog() *fakeCatalog { return &fakeCatalog{menus: map[string]fakeMenu{}} }

func (f *fakeCatalog) seed(merchantID string, version int64, items map[string]itemInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]itemInfo, len(items))
	for k, v := range items {
		cp[k] = v
	}
	f.menus[merchantID] = fakeMenu{version: version, items: cp}
}

func (f *fakeCatalog) fetchMenu(_ context.Context, merchantID string) (int64, map[string]itemInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	m, ok := f.menus[merchantID]
	if !ok {
		return 0, nil, errMerchantUnknown
	}
	cp := make(map[string]itemInfo, len(m.items))
	for k, v := range m.items {
		cp[k] = v
	}
	return m.version, cp, nil
}

// newTestServer builds an in-process cart server on in-memory SQLite with a
// ManualClock, an injected fake catalog, and cart_v1 forced on (the e2e/prod
// default is OFF; tests exercise the enabled path). No Docker, no external DB,
// no Redis — the ETag concurrency, the Redis-like snapshot, and the menu.updated
// revalidation are the real code paths.
func newTestServer(t *testing.T) (*server, *ManualClock, *fakeCatalog) {
	t.Helper()
	ctx := context.Background()
	clk := NewManualClock(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	view := newCatalogView()
	snap := newSnapshotStore(clk, 5*time.Second)
	st, err := openStore(ctx, "bkk", clk, view, snap)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(st.close)
	fc := newFakeCatalog()
	consumer := newMenuConsumer(view, "cart", func(ctx context.Context, merchantID string, version int64) error {
		_, err := st.revalidateMerchant(ctx, merchantID, version)
		return err
	})
	srv := &server{
		st: st, view: view, snap: snap, consumer: consumer, fetcher: fc,
		log:     logging.New(logging.Config{Service: "cart", Version: "test", Env: "test", Region: "bkk", SampleRate: 0, Out: &bytes.Buffer{}}),
		flags:   flags.NewSet(map[string]string{"cart_v1": "true"}),
		enabled: true,
	}
	return srv, clk, fc
}

// do issues a request against the server mux and returns (status, etag, body-map).
func do(t *testing.T, h http.Handler, method, path, ifMatch, body string) (int, string, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, rec.Header().Get("ETag"), m
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

func subtotal(m map[string]any) int64 {
	if st, ok := m["subtotal"].(map[string]any); ok {
		if a, ok := st["amount"].(float64); ok {
			return int64(a)
		}
	}
	return -1
}

// seedItem registers one available item for a merchant in the fake catalog.
func seedItem(fc *fakeCatalog, merchantID, itemID, name string, amount int64) {
	fc.seed(merchantID, 1, map[string]itemInfo{
		itemID: {Name: name, Amount: amount, Currency: "THB", Available: true, Version: 1},
	})
}

const (
	tMerchant = "mer_01test0000000000000000cart"
	tItem     = "itm_01test0000000000000000somt"
	tCart     = "crt_01test0000000000000000cart"
)

// addBody builds the POST /items body (item_id + additive merchant_id + quantity).
func addBody(itemID, merchantID string, qty int64) string {
	b, _ := json.Marshal(addInput{ItemID: itemID, MerchantID: merchantID, Quantity: qty})
	return string(b)
}

// --- tests ------------------------------------------------------------------

// TestAddGetRemove exercises the full cart lifecycle: first add creates the cart
// (v1, no If-Match) + prices from the catalog; GET returns the ETag; a second add
// with the fresh ETag bumps the version; remove with the fresh ETag empties it.
func TestAddGetRemove(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)

	// First add — creates the cart, no If-Match required.
	code, etag1, m := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 2))
	if code != 200 {
		t.Fatalf("first add: want 200, got %d (%v)", code, m)
	}
	if etag1 == "" {
		t.Fatal("first add returned no ETag")
	}
	if got := subtotal(m); got != 16000 {
		t.Fatalf("subtotal after add = %d, want 16000 (2 × 8000)", got)
	}

	// GET returns the same ETag and one line.
	code, getETag, m := do(t, h, http.MethodGet, "/v1/carts/"+tCart, "", "")
	if code != 200 || getETag != etag1 {
		t.Fatalf("get cart: code=%d etag=%q want %q", code, getETag, etag1)
	}
	if items := m["items"].([]any); len(items) != 1 {
		t.Fatalf("want 1 line, got %d", len(items))
	}

	// Second add of the SAME item with the fresh ETag → quantity accrues, version bumps.
	code, etag2, m := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag1, addBody(tItem, tMerchant, 1))
	if code != 200 || etag2 == "" || etag2 == etag1 {
		t.Fatalf("second add: code=%d etag old=%q new=%q", code, etag1, etag2)
	}
	if got := subtotal(m); got != 24000 {
		t.Fatalf("subtotal after 2nd add = %d, want 24000 (3 × 8000)", got)
	}

	// Remove the item with the current ETag → empty cart.
	code, etag3, m := do(t, h, http.MethodDelete, "/v1/carts/"+tCart+"/items/"+tItem, etag2, "")
	if code != 200 || etag3 == etag2 {
		t.Fatalf("remove: code=%d etag old=%q new=%q", code, etag2, etag3)
	}
	if items := m["items"].([]any); len(items) != 0 {
		t.Fatalf("want 0 lines after remove, got %d", len(items))
	}
	// Catalog was fetched exactly once (cold), then served from the view.
	if fc.calls != 1 {
		t.Fatalf("catalog fetches = %d, want exactly 1 (cold fetch, then cached view)", fc.calls)
	}
}

// TestStaleWrite412 is a core correctness property: an add/remove replaying a
// STALE If-Match is rejected 412 STALE_WRITE (02 §1).
func TestStaleWrite412(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)

	// Create + get the v1 ETag, then a good edit → v2 ETag.
	_, etag1, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	code, etag2, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag1, addBody(tItem, tMerchant, 1))
	if code != 200 || etag2 == etag1 {
		t.Fatalf("second add should mint a new ETag: %d old=%q new=%q", code, etag1, etag2)
	}

	// Replay the STALE (v1) ETag → 412 STALE_WRITE.
	code, _, m := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag1, addBody(tItem, tMerchant, 1))
	if code != http.StatusPreconditionFailed || errCode(m) != "STALE_WRITE" {
		t.Fatalf("stale add: want 412 STALE_WRITE, got %d %q", code, errCode(m))
	}
	// A stale REMOVE is also 412.
	code, _, m = do(t, h, http.MethodDelete, "/v1/carts/"+tCart+"/items/"+tItem, etag1, "")
	if code != http.StatusPreconditionFailed || errCode(m) != "STALE_WRITE" {
		t.Fatalf("stale remove: want 412 STALE_WRITE, got %d %q", code, errCode(m))
	}
}

// TestIfMatchRequired: a mutating add/remove on an EXISTING cart with no If-Match
// is refused (428). The first add (create) is exempt (bootstrap).
func TestIfMatchRequired(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)

	// Create (no If-Match) — allowed.
	code, _, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	if code != 200 {
		t.Fatalf("create add: want 200, got %d", code)
	}
	// Second add without If-Match → 428.
	code, _, m := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	if code != 428 || errCode(m) != "IF_MATCH_REQUIRED" {
		t.Fatalf("no If-Match add: want 428 IF_MATCH_REQUIRED, got %d %q", code, errCode(m))
	}
	// Remove without If-Match → 428.
	code, _, m = do(t, h, http.MethodDelete, "/v1/carts/"+tCart+"/items/"+tItem, "", "")
	if code != 428 || errCode(m) != "IF_MATCH_REQUIRED" {
		t.Fatalf("no If-Match remove: want 428 IF_MATCH_REQUIRED, got %d %q", code, errCode(m))
	}
}

// TestConcurrentAddFixture is the headline concurrency proof: N writers all
// holding the SAME cart ETag race to add; EXACTLY ONE commits and 100% of the
// stale writers are rejected with 412 STALE_WRITE (same guarantee V-T3 proved).
func TestConcurrentAddFixture(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)

	// Create the cart at v1, capture the shared ETag.
	_, etag1, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))

	const writers = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, stale, other := 0, 0, 0
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			// Every writer uses the SAME (v1) ETag — only one can win the CAS.
			code, _, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag1, addBody(tItem, tMerchant, 1))
			mu.Lock()
			switch code {
			case 200:
				ok++
			case http.StatusPreconditionFailed:
				stale++
			default:
				other++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if ok != 1 {
		t.Fatalf("want exactly 1 winning add, got %d", ok)
	}
	if stale != writers-1 {
		t.Fatalf("want %d stale writes rejected 412, got %d (other=%d)", writers-1, stale, other)
	}
	if other != 0 {
		t.Fatalf("unexpected non-412/non-200 responses: %d", other)
	}
}

// TestSequentialEditsChainETags: each accepted edit yields a fresh ETag usable
// for the next edit; the previous ETag is always stale.
func TestSequentialEditsChainETags(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)
	_, etag, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	for i := 0; i < 5; i++ {
		code, next, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag, addBody(tItem, tMerchant, 1))
		if code != 200 {
			t.Fatalf("edit %d: want 200, got %d", i, code)
		}
		if sc, _, _ := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", etag, addBody(tItem, tMerchant, 1)); sc != 412 {
			t.Fatalf("edit %d: reused ETag should be 412, got %d", i, sc)
		}
		etag = next
	}
}

// TestItemUnavailableRejected: adding an item the catalog marks unavailable →
// 422 ITEM_UNAVAILABLE; an item not on the menu → 422 ITEM_NOT_IN_MENU; an
// unknown merchant → 404 MERCHANT_UNKNOWN.
func TestItemUnavailableRejected(t *testing.T) {
	s, _, fc := newTestServer(t)
	h := s.mux()
	fc.seed(tMerchant, 1, map[string]itemInfo{
		tItem: {Name: "Som Tam", Amount: 8000, Currency: "THB", Available: false, Version: 1},
	})
	code, _, m := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	if code != 422 || errCode(m) != "ITEM_UNAVAILABLE" {
		t.Fatalf("unavailable add: want 422 ITEM_UNAVAILABLE, got %d %q", code, errCode(m))
	}
	code, _, m = do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody("itm_nope", tMerchant, 1))
	if code != 422 || errCode(m) != "ITEM_NOT_IN_MENU" {
		t.Fatalf("unknown item: want 422 ITEM_NOT_IN_MENU, got %d %q", code, errCode(m))
	}
	code, _, m = do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, "mer_unknown", 1))
	if code != 404 || errCode(m) != "MERCHANT_UNKNOWN" {
		t.Fatalf("unknown merchant: want 404 MERCHANT_UNKNOWN, got %d %q", code, errCode(m))
	}
}

// TestCartFlagGate: with cart_v1 OFF the mutating surface is dark (404
// CART_DISABLED).
func TestCartFlagGate(t *testing.T) {
	s, _, fc := newTestServer(t)
	s.enabled = false
	s.flags = flags.NewSet(map[string]string{"cart_v1": "false"})
	h := s.mux()
	seedItem(fc, tMerchant, tItem, "Som Tam", 8000)
	code, _, m := do(t, h, http.MethodPost, "/v1/carts/"+tCart+"/items", "", addBody(tItem, tMerchant, 1))
	if code != 404 || errCode(m) != "CART_DISABLED" {
		t.Fatalf("flag off: want 404 CART_DISABLED, got %d %q", code, errCode(m))
	}
}

// TestGetUnknownCart: GET on an absent cart → 404 CART_NOT_FOUND.
func TestGetUnknownCart(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	code, _, m := do(t, h, http.MethodGet, "/v1/carts/crt_nope", "", "")
	if code != 404 || errCode(m) != "CART_NOT_FOUND" {
		t.Fatalf("unknown cart: want 404 CART_NOT_FOUND, got %d %q", code, errCode(m))
	}
}
