package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/services/feed-cache/cache"
)

func testLogger() *logging.Logger {
	return logging.New(logging.Config{Service: "feed-cache", Version: "test", Env: "test", Region: "test", SampleRate: 1.0})
}

// originServer is a fake origin (ranking/catalog stand-in) that counts hits and
// echoes the request path so tests can assert the feed is served from cache
// (X-Cache) and that overrides are forwarded.
type originServer struct {
	hits     atomic.Int64
	lastFlag atomic.Value // string: last X-Flag-Override seen
}

func (o *originServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.hits.Add(1)
		o.lastFlag.Store(r.Header.Get(overrideHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `","hit":` + strconv.FormatInt(o.hits.Load(), 10) + `}`))
	})
}

// newTestServer builds a feed-cache server wired to a fake origin, with the given
// feed_cache default and short TTLs for deterministic assertions.
func newTestServer(originURL string, cacheOn bool, l1, l2, fresh, stale time.Duration) *server {
	feedOrig := cache.NewHTTPOrigin(func(tile string) string {
		lat, lng, _ := cache.TileCenter(tile)
		q := url.Values{}
		q.Set("lat", strconv.FormatFloat(lat, 'f', 5, 64))
		q.Set("lng", strconv.FormatFloat(lng, 'f', 5, 64))
		return originURL + "/v1/customer/home?" + q.Encode()
	}, overrideHeader)
	merchOrig := cache.NewHTTPOrigin(func(id string) string {
		return originURL + "/v1/merchants/" + url.PathEscape(id) + "/menu"
	}, overrideHeader)
	return &server{
		feed:     cache.NewFeedCache(cache.SystemClock{}, feedOrig, fresh, stale),
		merchant: cache.NewTwoTier(cache.SystemClock{}, merchOrig, l1, l2),
		log:      testLogger(),
		flags:    flags.NewSet(map[string]string{}),
		cacheOn:  cacheOn,
	}
}

func (s *server) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", s.handleHealth)
	m.HandleFunc("/v1/customer/home", s.only(http.MethodGet, s.handleFeed))
	m.HandleFunc("/v1/customer/merchants/", s.only(http.MethodGet, s.handleMerchant))
	m.HandleFunc("/v1/cache/stats", s.only(http.MethodGet, s.handleStats))
	return m
}

// TestHandleFeed_CacheHitOnRepeat: flag ON ⇒ first browse is a MISS (origin
// fetched), the repeat is a HIT (origin NOT fetched again).
func TestHandleFeed_CacheHitOnRepeat(t *testing.T) {
	origin := &originServer{}
	os := httptest.NewServer(origin.handler())
	defer os.Close()
	srv := newTestServer(os.URL, true, time.Second, 10*time.Second, time.Minute, time.Minute)
	h := srv.mux()

	c1 := getRec(t, h, "/v1/customer/home?lat=13.75&lng=100.50", nil)
	if c1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first browse X-Cache=%q, want MISS", c1.Header().Get("X-Cache"))
	}
	c2 := getRec(t, h, "/v1/customer/home?lat=13.75&lng=100.50", nil)
	if c2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("repeat browse X-Cache=%q, want HIT", c2.Header().Get("X-Cache"))
	}
	if origin.hits.Load() != 1 {
		t.Fatalf("origin hit %d times over 2 browses, want 1 (cached)", origin.hits.Load())
	}
}

// TestHandleFeed_OverrideBypassesAndForwards: an X-Flag-Override request bypasses
// the shared cache and forwards the override to the origin (so the V-T5 ranking_ml
// flip still reaches ranking, and cached values aren't reused across flag states).
func TestHandleFeed_OverrideBypassesAndForwards(t *testing.T) {
	origin := &originServer{}
	os := httptest.NewServer(origin.handler())
	defer os.Close()
	srv := newTestServer(os.URL, true, time.Second, 10*time.Second, time.Minute, time.Minute)
	h := srv.mux()

	hdr := http.Header{}
	hdr.Set(overrideHeader, "ranking_ml=false")
	rec := getRec(t, h, "/v1/customer/home?lat=13.75&lng=100.50", hdr)
	if rec.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("override browse X-Cache=%q, want BYPASS", rec.Header().Get("X-Cache"))
	}
	// Repeat override: still bypass, origin hit again (never cached).
	_ = getRec(t, h, "/v1/customer/home?lat=13.75&lng=100.50", hdr)
	if origin.hits.Load() != 2 {
		t.Fatalf("override requests cached (origin hits=%d, want 2)", origin.hits.Load())
	}
	if got, _ := origin.lastFlag.Load().(string); got != "ranking_ml=false" {
		t.Fatalf("override not forwarded to origin: %q", got)
	}
}

// TestHandleFeed_FlagOffPassthrough: feed_cache OFF ⇒ every request is a BYPASS
// (transparent passthrough), origin hit every time.
func TestHandleFeed_FlagOffPassthrough(t *testing.T) {
	origin := &originServer{}
	os := httptest.NewServer(origin.handler())
	defer os.Close()
	srv := newTestServer(os.URL, false, time.Second, 10*time.Second, time.Minute, time.Minute)
	h := srv.mux()
	for i := 0; i < 3; i++ {
		rec := getRec(t, h, "/v1/customer/home?lat=13.75&lng=100.50", nil)
		if rec.Header().Get("X-Cache") != "BYPASS" {
			t.Fatalf("flag-off browse %d X-Cache=%q, want BYPASS", i, rec.Header().Get("X-Cache"))
		}
	}
	if origin.hits.Load() != 3 {
		t.Fatalf("flag-off origin hits=%d, want 3 (no caching)", origin.hits.Load())
	}
}

// TestHandleMerchant_TwoTier: first merchant page is a MISS (origin), repeat is a
// HIT from L1 — the two-tier cache collapses catalog load.
func TestHandleMerchant_TwoTier(t *testing.T) {
	origin := &originServer{}
	os := httptest.NewServer(origin.handler())
	defer os.Close()
	srv := newTestServer(os.URL, true, time.Second, 10*time.Second, time.Minute, time.Minute)
	h := srv.mux()

	r1 := getRec(t, h, "/v1/customer/merchants/mer_abc", nil)
	if r1.Header().Get("X-Cache") != "MISS" || r1.Header().Get("X-Cache-Tier") != "origin" {
		t.Fatalf("first page X-Cache=%q tier=%q, want MISS/origin", r1.Header().Get("X-Cache"), r1.Header().Get("X-Cache-Tier"))
	}
	if !strings.Contains(r1.Body.String(), "/v1/merchants/mer_abc/menu") {
		t.Fatalf("merchant page body did not proxy the catalog menu path: %s", r1.Body.String())
	}
	r2 := getRec(t, h, "/v1/customer/merchants/mer_abc", nil)
	if r2.Header().Get("X-Cache") != "HIT" || r2.Header().Get("X-Cache-Tier") != "l1" {
		t.Fatalf("repeat page X-Cache=%q tier=%q, want HIT/l1", r2.Header().Get("X-Cache"), r2.Header().Get("X-Cache-Tier"))
	}
	if origin.hits.Load() != 1 {
		t.Fatalf("catalog origin hit %d times over 2 merchant reads, want 1", origin.hits.Load())
	}
}

// TestHandleStats returns feed + merchant stats with a hit rate.
func TestHandleStats(t *testing.T) {
	origin := &originServer{}
	os := httptest.NewServer(origin.handler())
	defer os.Close()
	srv := newTestServer(os.URL, true, time.Second, 10*time.Second, time.Minute, time.Minute)
	h := srv.mux()
	_ = getRec(t, h, "/v1/customer/merchants/mer_x", nil)
	_ = getRec(t, h, "/v1/customer/merchants/mer_x", nil)
	rec := getRec(t, h, "/v1/cache/stats", nil)
	var out struct {
		Merchant cache.TwoTierStats `json:"merchant"`
		Feed     cache.FeedStats    `json:"feed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("stats decode: %v (%s)", err, rec.Body.String())
	}
	if out.Merchant.OriginFetches != 1 || out.Merchant.Requests != 2 {
		t.Fatalf("merchant stats fetches=%d requests=%d, want 1/2", out.Merchant.OriginFetches, out.Merchant.Requests)
	}
	if out.Merchant.HitRate < 0.49 {
		t.Fatalf("merchant hit rate %.2f, want ~0.5", out.Merchant.HitRate)
	}
}

// TestHandleFeed_BadLatLng returns the 02 §2 error envelope.
func TestHandleFeed_BadLatLng(t *testing.T) {
	srv := newTestServer("http://127.0.0.1:0", true, time.Second, 10*time.Second, time.Minute, time.Minute)
	rec := getRec(t, srv.mux(), "/v1/customer/home?lat=x&lng=y", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad latlng code=%d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION") {
		t.Fatalf("want VALIDATION envelope, got %s", rec.Body.String())
	}
}

func getRec(t *testing.T, h http.Handler, path string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
