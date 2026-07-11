package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/services/search-indexer/index"
)

// newTestServer builds an in-process search-query server with search_v2 forced on
// and an embedded index node, mirroring main()'s wiring.
func newTestServer(t *testing.T, enabled bool) (*queryServer, *http.ServeMux) {
	t.Helper()
	node := index.NewNode(projectionGroup, index.EngineOptions{})
	t.Cleanup(node.Close)
	srv := &queryServer{
		node:    node,
		log:     logging.New(logging.Config{Service: "search-query", Env: "test", SampleRate: 0}),
		flags:   flags.NewSet(map[string]string{"search_v2": boolStr(enabled)}),
		enabled: enabled,
		region:  "bkk",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/search", srv.only(http.MethodGet, srv.handleSearch))
	mux.HandleFunc("/v1/customer/home", srv.only(http.MethodGet, srv.handleBrowse))
	mux.HandleFunc("/v1/index/merchants", srv.only(http.MethodPost, srv.handleIngestDoc))
	return srv, mux
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func seed(t *testing.T, h http.Handler) {
	t.Helper()
	code, _ := do(t, h, http.MethodPost, "/v1/index/merchants",
		`{"merchant_id":"mer_test","name":"Test Som Tam","lat":13.7563,"lng":100.5018,"open":true,"rating":4.8,"menu_version":1,"items":[{"item_id":"i1","name":"Som Tam","amount":8000,"currency":"THB","available":true}]}`)
	if code != http.StatusAccepted {
		t.Fatalf("seed ingest: got %d", code)
	}
}

func TestBrowseFeed(t *testing.T) {
	_, h := newTestServer(t, true)
	seed(t, h)
	code, body := do(t, h, http.MethodGet, "/v1/customer/home?lat=13.7563&lng=100.5018", "")
	if code != http.StatusOK {
		t.Fatalf("browse: got %d", code)
	}
	feed, _ := body["feed"].([]any)
	if len(feed) != 1 {
		t.Fatalf("browse feed len=%d, want 1 (%v)", len(feed), body)
	}
	item := feed[0].(map[string]any)
	if item["store_id"] != "mer_test" || item["rating"].(float64) != 4.8 {
		t.Fatalf("browse feed item wrong: %v", item)
	}
	if _, ok := item["delivery_fee"]; !ok {
		t.Fatalf("browse feed item missing delivery_fee: %v", item)
	}
}

func TestGeoSearch(t *testing.T) {
	_, h := newTestServer(t, true)
	seed(t, h)
	code, body := do(t, h, http.MethodGet, "/v1/search?lat=13.7563&lng=100.5018&q=som+tam", "")
	if code != http.StatusOK {
		t.Fatalf("search: got %d", code)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["store_id"] != "mer_test" {
		t.Fatalf("geo search wrong: %v", body)
	}
	// A far query returns nothing (H3-res-5 geo routing).
	_, far := do(t, h, http.MethodGet, "/v1/search?lat=18.79&lng=98.99&q=som+tam", "")
	if r, _ := far["results"].([]any); len(r) != 0 {
		t.Fatalf("far query should be empty, got %v", far)
	}
}

func TestFlagGate(t *testing.T) {
	_, h := newTestServer(t, false) // search_v2 OFF
	code, body := do(t, h, http.MethodGet, "/v1/customer/home?lat=13.7&lng=100.5", "")
	if code != http.StatusNotFound {
		t.Fatalf("flag off: browse got %d, want 404", code)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "SEARCH_DISABLED" {
		t.Fatalf("flag off: want SEARCH_DISABLED, got %v", body)
	}
}

func TestLatLngValidation(t *testing.T) {
	_, h := newTestServer(t, true)
	code, body := do(t, h, http.MethodGet, "/v1/search?lat=abc", "")
	if code != http.StatusBadRequest {
		t.Fatalf("bad lat/lng: got %d, want 400", code)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "VALIDATION" {
		t.Fatalf("bad lat/lng: want VALIDATION, got %v", body)
	}
}
