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

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
)

// newTestServer builds an in-process merchant-catalog server on in-memory SQLite
// with catalog_v1 forced on (the e2e/prod default is OFF; tests exercise the
// enabled path). No Docker, no external DB — the outbox + ETag concurrency are
// the real code paths.
func newTestServer(t *testing.T) *server {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx, "bkk")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(st.close)
	return &server{
		st: st, ev: newEventBuilder("bkk"),
		log:     logging.New(logging.Config{Service: "merchant-catalog", Version: "test", Env: "test", Region: "bkk", SampleRate: 0, Out: &bytes.Buffer{}}),
		flags:   flags.NewSet(map[string]string{"catalog_v1": "true"}),
		enabled: true,
	}
}

// handler rebuilds the same mux as main.go (kept in sync manually, as
// identity-profile does).
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/merchants", s.only(http.MethodPost, s.handleCreateMerchant))
	mux.HandleFunc("/v1/merchants/", s.handleMerchantSubtree)
	return mux
}

// do issues a request and returns (status, etag, decoded-body-map).
func do(t *testing.T, h http.Handler, method, path, ifMatch, body string) (int, string, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
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

func createMerchant(t *testing.T, h http.Handler, id string) (string, string) {
	t.Helper()
	body := `{"merchant_id":"` + id + `","name":"Som Tam House"}`
	code, _, m := do(t, h, http.MethodPost, "/v1/merchants", "", body)
	if code != http.StatusCreated {
		t.Fatalf("create merchant: want 201, got %d (%v)", code, m)
	}
	menu := m["menu"].(map[string]any)
	store := m["store_status"].(map[string]any)
	return menu["etag"].(string), store["etag"].(string)
}

// TestMenuCRUD exercises create → read → edit → read with ETag propagation.
func TestMenuCRUD(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test0000000000000000crud"
	menuETag, _ := createMerchant(t, h, mid)

	// GET menu returns the same ETag and an empty item list.
	code, getETag, m := do(t, h, http.MethodGet, "/v1/merchants/"+mid+"/menu", "", "")
	if code != 200 || getETag == "" {
		t.Fatalf("get menu: code=%d etag=%q", code, getETag)
	}
	if getETag != menuETag {
		t.Fatalf("get menu etag %q != create etag %q", getETag, menuETag)
	}
	if items := m["items"].([]any); len(items) != 0 {
		t.Fatalf("new menu should be empty, got %d items", len(items))
	}

	// PATCH: add an item with the correct If-Match → 200, new ETag, item present.
	patch := `{"upsert_items":[{"name":"Som Tam","price":{"amount":8000,"currency":"THB"},"available":true}]}`
	code, newETag, m := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag, patch)
	if code != 200 {
		t.Fatalf("patch add item: want 200, got %d (%v)", code, m)
	}
	if newETag == "" || newETag == menuETag {
		t.Fatalf("patch should mint a NEW etag; old=%q new=%q", menuETag, newETag)
	}
	items := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 item after add, got %d", len(items))
	}
	it := items[0].(map[string]any)
	if it["name"] != "Som Tam" {
		t.Fatalf("item name = %v", it["name"])
	}

	// The stale (original) ETag must now be rejected on a second edit → 412.
	code, _, m = do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag, patch)
	if code != http.StatusPreconditionFailed {
		t.Fatalf("stale patch: want 412, got %d (%v)", code, m)
	}
	if errCode(m) != "STALE_WRITE" {
		t.Fatalf("stale patch code = %q, want STALE_WRITE", errCode(m))
	}
}

// TestIfMatchRequired: a mutating PATCH/PUT with no If-Match is refused (428).
func TestIfMatchRequired(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test000000000000000ifmatch"
	createMerchant(t, h, mid)

	code, _, m := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", "", `{"upsert_items":[]}`)
	if code != 428 || errCode(m) != "IF_MATCH_REQUIRED" {
		t.Fatalf("no If-Match menu: want 428 IF_MATCH_REQUIRED, got %d %q", code, errCode(m))
	}
	code, _, m = do(t, h, http.MethodPut, "/v1/merchants/"+mid+"/store-status", "", `{"status":"OPEN"}`)
	if code != 428 || errCode(m) != "IF_MATCH_REQUIRED" {
		t.Fatalf("no If-Match status: want 428 IF_MATCH_REQUIRED, got %d %q", code, errCode(m))
	}
}

// TestStoreStatusConcurrency: store-status PUT is under the same 412 rule.
func TestStoreStatusConcurrency(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test000000000000000status"
	_, statusETag := createMerchant(t, h, mid)

	code, newETag, m := do(t, h, http.MethodPut, "/v1/merchants/"+mid+"/store-status", statusETag, `{"status":"OPEN"}`)
	if code != 200 || m["status"] != "OPEN" {
		t.Fatalf("set OPEN: want 200 OPEN, got %d (%v)", code, m)
	}
	if newETag == statusETag {
		t.Fatalf("status etag should change on write")
	}
	// Stale write with the original ETag → 412.
	code, _, m = do(t, h, http.MethodPut, "/v1/merchants/"+mid+"/store-status", statusETag, `{"status":"BUSY"}`)
	if code != http.StatusPreconditionFailed || errCode(m) != "STALE_WRITE" {
		t.Fatalf("stale status: want 412 STALE_WRITE, got %d %q", code, errCode(m))
	}
	// Invalid status value → 400 VALIDATION.
	code, _, m = do(t, h, http.MethodPut, "/v1/merchants/"+mid+"/store-status", newETag, `{"status":"PARTY"}`)
	if code != 400 || errCode(m) != "VALIDATION" {
		t.Fatalf("bad status: want 400 VALIDATION, got %d %q", code, errCode(m))
	}
}

// TestConcurrentEditFixture is the headline test-criterion: under N concurrent
// writers that all read the SAME menu ETag and then race to PATCH, EXACTLY ONE
// commits and 100% of the stale writers are rejected with 412 STALE_WRITE.
func TestConcurrentEditFixture(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test0000000000000concurr"
	menuETag, _ := createMerchant(t, h, mid)

	const writers = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, stale, other := 0, 0, 0
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			patch := `{"upsert_items":[{"name":"Racer","price":{"amount":100,"currency":"THB"}}]}`
			// Every writer uses the SAME (v1) ETag — only one can win.
			code, _, m := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag, patch)
			mu.Lock()
			switch {
			case code == 200:
				ok++
			case code == http.StatusPreconditionFailed && errCode(m) == "STALE_WRITE":
				stale++
			default:
				other++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if ok != 1 {
		t.Fatalf("want exactly 1 winning write, got %d", ok)
	}
	if stale != writers-1 {
		t.Fatalf("want %d stale writes rejected with 412, got %d (other=%d)", writers-1, stale, other)
	}
	if other != 0 {
		t.Fatalf("unexpected non-412/non-200 responses: %d", other)
	}
	// After the storm the menu has exactly ONE item (the single winner's insert).
	_, _, m := do(t, h, http.MethodGet, "/v1/merchants/"+mid+"/menu", "", "")
	if items := m["items"].([]any); len(items) != 1 {
		t.Fatalf("want 1 item after concurrent storm, got %d", len(items))
	}
}

// TestSequentialEditsChainETags: each accepted edit yields a fresh ETag usable
// for the next edit; the previous ETag is always stale.
func TestSequentialEditsChainETags(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test00000000000000000seq"
	etag, _ := createMerchant(t, h, mid)
	for i := 0; i < 5; i++ {
		patch := `{"upsert_items":[{"name":"I","price":{"amount":1,"currency":"THB"}}]}`
		code, next, _ := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", etag, patch)
		if code != 200 {
			t.Fatalf("edit %d: want 200, got %d", i, code)
		}
		// Old etag is now stale.
		if sc, _, _ := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", etag, patch); sc != 412 {
			t.Fatalf("edit %d: reused etag should be 412, got %d", i, sc)
		}
		etag = next
	}
}

// TestEventsPublishedThroughOutbox proves every accepted mutation writes a
// schema-valid event to the transactional outbox: create → 1 menu.updated +
// 1 store.status_changed; menu edit → menu.updated; status set →
// store.status_changed. Keys are merchant_id; payloads carry the full snapshot.
func TestEventsPublishedThroughOutbox(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test0000000000000000event"
	menuETag, statusETag := createMerchant(t, h, mid)

	// One menu edit + one status change.
	_, newMenuETag, _ := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag,
		`{"upsert_items":[{"name":"Larb","price":{"amount":9000,"currency":"THB"}}]}`)
	if newMenuETag == "" {
		t.Fatal("menu edit produced no etag")
	}
	if code, _, m := do(t, h, http.MethodPut, "/v1/merchants/"+mid+"/store-status", statusETag, `{"status":"OPEN"}`); code != 200 {
		t.Fatalf("status set failed: %d %v", code, m)
	}

	recs, err := s.st.ob.Tail(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("outbox tail: %v", err)
	}
	// create=2, menu edit=1, status set=1 → 4 events total.
	if len(recs) != 4 {
		t.Fatalf("want 4 outbox events, got %d", len(recs))
	}
	var menuUpdated, statusChanged int
	for _, r := range recs {
		env, err := eventbus.UnmarshalEnvelope(r.Raw)
		if err != nil {
			t.Fatalf("bad envelope: %v", err)
		}
		if env.Aggregate.ID != mid {
			t.Fatalf("event key (aggregate.id) = %q, want merchant_id %q", env.Aggregate.ID, mid)
		}
		if r.Key != mid {
			t.Fatalf("outbox partition key = %q, want %q", r.Key, mid)
		}
		if env.Aggregate.Type != "merchant" {
			t.Fatalf("aggregate.type = %q, want merchant", env.Aggregate.Type)
		}
		switch env.EventType {
		case "menu.updated":
			menuUpdated++
			var p menuUpdatedPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				t.Fatalf("menu payload: %v", err)
			}
			if p.MerchantID != mid {
				t.Fatalf("menu payload merchant_id = %q", p.MerchantID)
			}
		case "store.status_changed":
			statusChanged++
			var p storeStatusPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				t.Fatalf("status payload: %v", err)
			}
			if p.Status == "" {
				t.Fatal("status payload missing status")
			}
		default:
			t.Fatalf("unexpected topic %q", env.EventType)
		}
	}
	if menuUpdated != 2 || statusChanged != 2 {
		t.Fatalf("want 2 menu.updated + 2 store.status_changed, got %d + %d", menuUpdated, statusChanged)
	}
}

// TestFailedWriteEmitsNoEvent: a stale (412) edit must NOT leave an outbox row —
// the event and the write share one transaction, so a rejected write publishes
// nothing (exactly-once / no phantom events).
func TestFailedWriteEmitsNoEvent(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test000000000000000noevt"
	menuETag, _ := createMerchant(t, h, mid)

	before, _ := s.st.ob.Tail(context.Background(), 0, 1000)
	// Land one good edit (bumps version), then replay the STALE etag → 412.
	do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag, `{"upsert_items":[{"name":"A","price":{"amount":1,"currency":"THB"}}]}`)
	afterGood, _ := s.st.ob.Tail(context.Background(), 0, 1000)
	sc, _, _ := do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag, `{"upsert_items":[{"name":"B","price":{"amount":1,"currency":"THB"}}]}`)
	if sc != 412 {
		t.Fatalf("expected stale 412, got %d", sc)
	}
	afterStale, _ := s.st.ob.Tail(context.Background(), 0, 1000)

	if len(afterGood) != len(before)+1 {
		t.Fatalf("good edit should add exactly 1 event: before=%d after=%d", len(before), len(afterGood))
	}
	if len(afterStale) != len(afterGood) {
		t.Fatalf("stale (412) edit must add NO event: %d -> %d", len(afterGood), len(afterStale))
	}
}

// TestCatalogFlagGate: with catalog_v1 OFF the mutating surface is dark (404
// CATALOG_DISABLED); reads are unaffected only after a merchant exists, so we
// assert the gate on create.
func TestCatalogFlagGate(t *testing.T) {
	s := newTestServer(t)
	s.enabled = false
	s.flags = flags.NewSet(map[string]string{"catalog_v1": "false"})
	h := s.handler()
	code, _, m := do(t, h, http.MethodPost, "/v1/merchants", "", `{"name":"X"}`)
	if code != 404 || errCode(m) != "CATALOG_DISABLED" {
		t.Fatalf("flag off: want 404 CATALOG_DISABLED, got %d %q", code, errCode(m))
	}
}

// TestNotFound: operations on an unknown merchant → 404 MERCHANT_NOT_FOUND.
func TestNotFound(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	code, _, m := do(t, h, http.MethodGet, "/v1/merchants/mer_nope/menu", "", "")
	if code != 404 || errCode(m) != "MERCHANT_NOT_FOUND" {
		t.Fatalf("unknown merchant menu: want 404 MERCHANT_NOT_FOUND, got %d %q", code, errCode(m))
	}
}
