package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/services/ranking/rank"
)

// fakeSource is a static CandidateSource so handler tests need no live search.
type fakeSource struct {
	cands []rank.Candidate
	err   error
}

func (f fakeSource) Candidates(_ context.Context, _, _ float64, _ int) ([]rank.Candidate, error) {
	return f.cands, f.err
}

// twoStores: B is higher-rated (static winner); A is popular via features (ML winner).
func twoStores() []rank.Candidate {
	return []rank.Candidate{
		{StoreID: "mer_b_highrated", Name: "B", Rating: 4.5, DistanceM: 500, Open: true,
			DeliveryFee: rank.Money{Amount: 1500, Currency: "THB"}, ETAMinutes: 15},
		{StoreID: "mer_a_popular", Name: "A", Rating: 4.0, DistanceM: 500, Open: true,
			DeliveryFee: rank.Money{Amount: 1500, Currency: "THB"}, ETAMinutes: 15},
	}
}

func newTestServer(t *testing.T, mlDefault bool, src rank.CandidateSource) (*server, *http.ServeMux) {
	t.Helper()
	node := rank.NewNode("ranking-test", rank.Options{})
	t.Cleanup(node.Close)
	// Make A popular so ML re-rank promotes it above higher-rated B.
	node.Features.Apply("mer_a_popular", rank.SignalOrder, 10)
	srv := &server{
		node:      node,
		source:    src,
		log:       logging.New(logging.Config{Service: "ranking", Env: "test", SampleRate: 0}),
		flags:     flags.NewSet(map[string]string{"ranking_ml": boolStr(mlDefault)}),
		mlDefault: mlDefault,
		region:    "bkk",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/customer/home", srv.only(http.MethodGet, srv.handleBrowse))
	mux.HandleFunc("/v1/rank", srv.only(http.MethodPost, srv.handleRank))
	mux.HandleFunc("/v1/signals/events", srv.only(http.MethodPost, srv.handleSignal))
	mux.HandleFunc("/v1/rank/stats", srv.only(http.MethodGet, srv.handleStats))
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

func topStore(body map[string]any) string {
	feed, _ := body["feed"].([]any)
	if len(feed) == 0 {
		return ""
	}
	item, _ := feed[0].(map[string]any)
	s, _ := item["store_id"].(string)
	return s
}

// TestBrowse_BothFlagStates is the V-T5 "both flag states demoed via the browse
// BFF endpoint" property, at the handler level: ranking_ml ON produces the ML
// order (popular A first); ranking_ml OFF produces the static fallback order
// (higher-rated B first). Both return 200 with a valid feed; the top store differs.
func TestBrowse_BothFlagStates(t *testing.T) {
	// Flag ON (ML re-rank).
	_, hOn := newTestServer(t, true, fakeSource{cands: twoStores()})
	codeOn, onBody := do(t, hOn, http.MethodGet, "/v1/customer/home?lat=13.7563&lng=100.5018", "")
	if codeOn != http.StatusOK {
		t.Fatalf("browse ML-on: got %d", codeOn)
	}
	if top := topStore(onBody); top != "mer_a_popular" {
		t.Fatalf("ranking_ml ON: expected popular A first, got %q (%v)", top, onBody)
	}
	if meta, _ := onBody["ranking"].(map[string]any); meta["scorer"] != "ml" {
		t.Fatalf("ranking_ml ON: expected scorer=ml, got %v", onBody["ranking"])
	}

	// Flag OFF (static fallback).
	_, hOff := newTestServer(t, false, fakeSource{cands: twoStores()})
	codeOff, offBody := do(t, hOff, http.MethodGet, "/v1/customer/home?lat=13.7563&lng=100.5018", "")
	if codeOff != http.StatusOK {
		t.Fatalf("browse ML-off: got %d", codeOff)
	}
	if top := topStore(offBody); top != "mer_b_highrated" {
		t.Fatalf("ranking_ml OFF: expected higher-rated B first (static), got %q (%v)", top, offBody)
	}
	if meta, _ := offBody["ranking"].(map[string]any); meta["scorer"] != "static" {
		t.Fatalf("ranking_ml OFF: expected scorer=static, got %v", offBody["ranking"])
	}

	if topStore(onBody) == topStore(offBody) {
		t.Fatal("ML and static feeds are indistinguishable — both flag states must differ")
	}
}

// TestRankEndpoint exercises the self-contained POST /v1/rank contract surface.
func TestRankEndpoint(t *testing.T) {
	_, h := newTestServer(t, true, fakeSource{})
	reqBody := `{"location":{"lat":13.7,"lng":100.5},"candidates":[` +
		`{"store_id":"mer_b_highrated","rating":4.5,"distance_m":500,"open":true},` +
		`{"store_id":"mer_a_popular","rating":4.0,"distance_m":500,"open":true}],"top_k":50}`
	code, body := do(t, h, http.MethodPost, "/v1/rank", reqBody)
	if code != http.StatusOK {
		t.Fatalf("rank: got %d (%v)", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("rank: expected 2 results, got %d", len(results))
	}
	if body["scorer"] != "ml" {
		t.Fatalf("rank: expected scorer=ml, got %v", body["scorer"])
	}
	first := results[0].(map[string]any)
	if first["store_id"] != "mer_a_popular" {
		t.Fatalf("rank: expected popular A first, got %v", first["store_id"])
	}
}

// TestSignalIngest_UpdatesFeatures proves the HTTP signal-ingest path feeds the
// feature store (event -> feature -> re-rank), end-to-end through the service.
func TestSignalIngest_UpdatesFeatures(t *testing.T) {
	srv, h := newTestServer(t, true, fakeSource{cands: twoStores()})
	// A fresh merchant with no features starts unpopular.
	env := `{"event_id":"evt_sig_1","event_type":"ranking.signal","occurred_at":"2026-01-01T00:00:00Z","trace_id":"t","aggregate":{"type":"merchant","id":"mer_new","region":"bkk"},"schema_version":1,"payload":{"merchant_id":"mer_new","signal_type":"order","weight":25}}`
	code, body := do(t, h, http.MethodPost, "/v1/signals/events", env)
	if code != http.StatusAccepted {
		t.Fatalf("signal ingest: got %d (%v)", code, body)
	}
	// Wait for async delivery, then assert the feature landed.
	waitMerchants(t, srv, 2) // mer_a_popular (seeded) + mer_new
	if srv.node.Features.Popularity("mer_new") <= 0 {
		t.Fatalf("signal ingest did not populate the feature store for mer_new")
	}
}

// TestBrowse_RetrievalFailure proves a failed candidate retrieval surfaces the
// 502 RANKING_RETRIEVAL_FAILED envelope (observable, not a silent empty feed).
func TestBrowse_RetrievalFailure(t *testing.T) {
	_, h := newTestServer(t, true, fakeSource{err: errors.New("search down")})
	code, body := do(t, h, http.MethodGet, "/v1/customer/home?lat=13.7&lng=100.5", "")
	if code != http.StatusBadGateway {
		t.Fatalf("retrieval failure: got %d, want 502", code)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "RANKING_RETRIEVAL_FAILED" {
		t.Fatalf("expected RANKING_RETRIEVAL_FAILED, got %v", body)
	}
}

func TestLatLngValidation(t *testing.T) {
	_, h := newTestServer(t, true, fakeSource{cands: twoStores()})
	code, body := do(t, h, http.MethodGet, "/v1/customer/home?lat=abc", "")
	if code != http.StatusBadRequest {
		t.Fatalf("bad lat/lng: got %d, want 400", code)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "VALIDATION" {
		t.Fatalf("bad lat/lng: want VALIDATION, got %v", body)
	}
}

func waitMerchants(t *testing.T, s *server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.node.Features.Merchants() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("feature store never reached %d merchants (got %d)", want, s.node.Features.Merchants())
}
