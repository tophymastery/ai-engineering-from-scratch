// Command feed-cache is the V-T6 Feed & merchant-page caches slice (decisions
// D11 + D17). It sits in front of the discovery read path and protects two hot
// origins with stampede-safe caches, wired into the customer-bff browse + merchant
// endpoints:
//
//	GET /v1/customer/home?lat=&lng=       geo-tile FEED cache (stale-while-
//	                                      revalidate, CDN-fronted); origin = the
//	                                      ranking browse feed (ranking→search).
//	GET /v1/customer/merchants/{id}       merchant-page TWO-TIER cache (in-process
//	                                      singleflight 1s over Redis 10s, D11);
//	                                      origin = merchant-catalog.
//	GET /v1/cache/stats                   hit-rate + origin-fetch telemetry.
//	GET /healthz
//
// The `feed_cache` flag gates the behaviour: ON = cache (hit + SWR revalidation
// paths); OFF = transparent passthrough to the origin. A request carrying an
// X-Flag-Override header BYPASSES the shared cache entirely — deterministic-test
// requests must neither read nor pollute it, and the override is forwarded to the
// origin (so e.g. the browse ranking_ml flip still reaches ranking).
//
// Sandbox adaptations (disclosed in VERIFICATION.md §V-T6): the "Redis 10s" tier
// is an in-process TTL store standing in for Redis (no daemon here); CDN-fronting
// is expressed in deploy/ manifests verified render-only. The singleflight +
// two-tier + SWR LOGIC — the correctness of this slice — is real and fully tested
// (services/feed-cache/cache, exactly-1-origin-fetch under 10k concurrent -race).
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
	"github.com/shop-platform/shop/services/feed-cache/cache"
)

const overrideHeader = "X-Flag-Override"

var codeOriginFailed = shoperr.Register("FEED_CACHE_ORIGIN_FAILED", 502, true, "Upstream origin fetch failed.")

type server struct {
	feed        *cache.FeedCache
	merchant    *cache.TwoTier
	feedOrigin  string
	merchOrigin string
	log         *logging.Logger
	flags       *flags.Set
	cacheOn     bool
}

func main() {
	port := envOr("PORT", "8116")
	name := envOr("SERVICE_NAME", "feed-cache")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	// Origins: the ranking browse feed (D17 two-phase retrieval→re-rank) and
	// merchant-catalog. In the E2E env these point at the ranking + catalog slots.
	feedOrigin := envOr("ORIGIN_FEED_URL", "http://localhost:8115")      // ranking
	merchOrigin := envOr("ORIGIN_MERCHANT_URL", "http://localhost:8102") // merchant-catalog

	// TTLs (D11/D17 defaults; override via env for the E2E revalidation demo).
	feedFresh := envDuration("FEED_FRESH_TTL", 30*time.Second)
	feedStale := envDuration("FEED_STALE_TTL", 5*time.Minute)
	l1TTL := envDuration("MERCHANT_L1_TTL", 1*time.Second)  // in-process singleflight tier
	l2TTL := envDuration("MERCHANT_L2_TTL", 10*time.Second) // Redis-like tier

	fs := flags.FromEnv()
	clk := cache.SystemClock{}

	// Feed origin fetches at the TILE CENTER so the tile key round-trips to one
	// origin request (all users in a tile share the tile's feed).
	feedOrig := cache.NewHTTPOrigin(func(tile string) string {
		lat, lng, ok := cache.TileCenter(tile)
		if !ok {
			return feedOrigin + "/v1/customer/home"
		}
		q := url.Values{}
		q.Set("lat", strconv.FormatFloat(lat, 'f', 5, 64))
		q.Set("lng", strconv.FormatFloat(lng, 'f', 5, 64))
		q.Set("limit", "500")
		return feedOrigin + "/v1/customer/home?" + q.Encode()
	}, overrideHeader)

	merchOrig := cache.NewHTTPOrigin(func(id string) string {
		return merchOrigin + "/v1/merchants/" + url.PathEscape(id) + "/menu"
	}, overrideHeader)

	srv := &server{
		feed:        cache.NewFeedCache(clk, feedOrig, feedFresh, feedStale),
		merchant:    cache.NewTwoTier(clk, merchOrig, l1TTL, l2TTL),
		feedOrigin:  feedOrigin,
		merchOrigin: merchOrigin,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		cacheOn: fs.Bool("feed_cache", false),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/customer/home", srv.only(http.MethodGet, srv.handleFeed))
	mux.HandleFunc("/v1/customer/merchants/", srv.only(http.MethodGet, srv.handleMerchant))
	mux.HandleFunc("/v1/cache/stats", srv.only(http.MethodGet, srv.handleStats))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))
	addr := ":" + port
	log.Printf("feed-cache %q on %s (env=%s region=%s feed_cache=%v feed_origin=%s merchant_origin=%s l1=%s l2=%s fresh=%s stale=%s)",
		name, addr, env, region, srv.cacheOn, feedOrigin, merchOrigin, l1TTL, l2TTL, feedFresh, feedStale)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("feed-cache server exited: %v", err)
	}
}

// cacheEnabled resolves the per-request feed_cache flag (X-Flag-Override in
// non-prod, else the env default).
func (s *server) cacheEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "feed_cache", s.cacheOn)
}

// bypass reports whether this request must skip the shared cache: either the
// feed_cache flag is OFF, or the request carries a per-request flag override
// (deterministic-test requests must not read/write the shared cache, and the
// override must be forwarded to the origin unshared).
func (s *server) bypass(r *http.Request) bool {
	if r.Header.Get(overrideHeader) != "" {
		return true
	}
	return !s.cacheEnabled(r)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "feed-cache",
		"feed_cache":      s.cacheEnabled(r),
		"feed_origin":     s.feedOrigin,
		"merchant_origin": s.merchOrigin,
		"otel_exporter":   otel.ExporterMode(),
	})
}

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	lat, lng, ok := s.latLng(w, r)
	if !ok {
		return
	}
	tile := cache.TileFor(lat, lng)
	if s.bypass(r) {
		res, err := s.feed.Bypass(r.Context(), tile, r.Header)
		s.writeCached(w, r, res.Value, "BYPASS", err)
		return
	}
	res, err := s.feed.Get(r.Context(), tile, r.Header)
	if err != nil {
		s.fail(w, r, shoperr.New(codeOriginFailed, err.Error()))
		return
	}
	xcache := "HIT"
	switch res.State {
	case "miss":
		xcache = "MISS"
	case "stale":
		xcache = "STALE"
	}
	w.Header().Set("X-Cache", xcache)
	w.Header().Set("X-Cache-Tile", tile)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Value)
}

func (s *server) handleMerchant(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/customer/merchants/")
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "merchant id path segment required"))
		return
	}
	if s.bypass(r) {
		res, err := s.merchant.Bypass(r.Context(), id, r.Header)
		s.writeCached(w, r, res.Value, "BYPASS", err)
		return
	}
	res, err := s.merchant.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, shoperr.New(codeOriginFailed, err.Error()))
		return
	}
	// Tier → X-Cache: l1/l2 are cache hits; origin is a (coalesced) fill.
	xcache := "HIT"
	if res.Tier == "origin" {
		xcache = "MISS"
	}
	w.Header().Set("X-Cache", xcache)
	w.Header().Set("X-Cache-Tier", res.Tier)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Value)
}

func (s *server) writeCached(w http.ResponseWriter, r *http.Request, body []byte, xcache string, err error) {
	if err != nil {
		s.fail(w, r, shoperr.New(codeOriginFailed, err.Error()))
		return
	}
	w.Header().Set("X-Cache", xcache)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"feed":     s.feed.Stats(),
		"merchant": s.merchant.Stats(),
	})
}

func (s *server) latLng(w http.ResponseWriter, r *http.Request) (float64, float64, bool) {
	q := r.URL.Query()
	lat, err1 := strconv.ParseFloat(q.Get("lat"), 64)
	lng, err2 := strconv.ParseFloat(q.Get("lng"), 64)
	if q.Get("lat") == "" || q.Get("lng") == "" || err1 != nil || err2 != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "lat and lng are required numeric query params",
			shoperr.Detail{Field: "lat", Reason: "required"}, shoperr.Detail{Field: "lng", Reason: "required"}))
		return 0, 0, false
	}
	return lat, lng, true
}

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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
