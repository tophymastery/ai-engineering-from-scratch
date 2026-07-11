// Command search-query is the query half of the V-T4 Search & browse slice
// (01 §1 `search`; decisions D17 + D11). It serves the read path — geo search and
// the customer browse feed — over the discovery index. The customer-bff browse
// endpoint `GET /v1/customer/home?lat=&lng=` (02 §4.2) aggregates search hits
// (this service) with fee + rating enrichment; the gateway routes it here as a
// BFF passthrough (like the V-T3 merchant-catalog passthrough), and the whole
// public surface is gated by the `search_v2` flag (ships dark, E2E runs it on).
//
// In production search-query and search-indexer are two deployments over a shared
// per-cell OpenSearch. This sandbox has no OpenSearch and no cross-process shared
// store, so search-query EMBEDS an index.Node (the same indexer code) and is fed
// through its ingest endpoints — that store adaptation is disclosed in
// VERIFICATION.md §V-T4. The routing, salting, debounce and backpressure it
// exercises are the identical index-package code the indexer service runs.
//
// HTTP surface:
//
//	GET  /healthz
//	GET  /v1/search?lat=&lng=&q=            geo(+text) search (search_v2)
//	GET  /v1/customer/home?lat=&lng=        browse feed (search_v2)
//	POST /v1/index/events                   ingest an event envelope (demo/freshness)
//	POST /v1/index/merchants               upsert a search document directly (seed)
//	GET  /v1/index/stats                    index stats
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
	"github.com/shop-platform/shop/services/search-indexer/index"
)

const projectionGroup = "search-projection"

var codeSearchDisabled = shoperr.Register("SEARCH_DISABLED", 404, false, "The search_v2 feature is not enabled.")

type queryServer struct {
	node    *index.Node
	log     *logging.Logger
	flags   *flags.Set
	enabled bool
	region  string
}

func main() {
	port := envOr("PORT", "8103")
	name := envOr("SERVICE_NAME", "search-query")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	fs := flags.FromEnv()
	node := index.NewNode(projectionGroup, index.EngineOptions{})
	srv := &queryServer{
		node: node,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("search_v2", false),
		region:  region,
	}
	go srv.ratingFlushLoop(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/search", srv.only(http.MethodGet, srv.handleSearch))
	mux.HandleFunc("/v1/customer/home", srv.only(http.MethodGet, srv.handleBrowse))
	mux.HandleFunc("/v1/index/events", srv.only(http.MethodPost, srv.handlePublishEvent))
	mux.HandleFunc("/v1/index/merchants", srv.only(http.MethodPost, srv.handleIngestDoc))
	mux.HandleFunc("/v1/index/stats", srv.only(http.MethodGet, srv.handleStats))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))
	addr := ":" + port
	log.Printf("search-query %q on %s (env=%s region=%s search_v2=%v)", name, addr, env, region, srv.enabled)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("search-query server exited: %v", err)
	}
}

func (s *queryServer) ratingFlushLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.node.Engine.FlushRatings(time.Now().UTC())
		}
	}
}

func (s *queryServer) searchEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "search_v2", s.enabled)
}

func (s *queryServer) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.searchEnabled(r) {
		s.fail(w, r, shoperr.New(codeSearchDisabled, ""))
		return false
	}
	return true
}

func (s *queryServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "search-query",
		"search_v2":     s.searchEnabled(r),
		"docs":          s.node.Engine.DocCount(),
		"otel_exporter": otel.ExporterMode(),
	})
}

// searchResults is the /v1/search response (search.v1 contract: results + cursor).
type searchResults struct {
	Results    []hitView `json:"results"`
	NextCursor *string   `json:"next_cursor"`
}

type hitView struct {
	StoreID   string  `json:"store_id"`
	Name      string  `json:"name"`
	Rating    float64 `json:"rating"`
	DistanceM int     `json:"distance_m"`
	Open      bool    `json:"open"`
}

func (s *queryServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	lat, lng, ok := latLng(w, r, s)
	if !ok {
		return
	}
	q := index.Query{Lat: lat, Lng: lng, Text: r.URL.Query().Get("q"), Limit: 20}
	hits := s.node.Engine.Search(q)
	out := searchResults{Results: make([]hitView, 0, len(hits))}
	for _, h := range hits {
		out.Results = append(out.Results, hitView{StoreID: h.StoreID, Name: h.Name, Rating: h.Rating, DistanceM: h.DistanceM, Open: h.Open})
	}
	writeJSON(w, http.StatusOK, out)
}

// homeFeed is the GET /v1/customer/home browse payload (02 §4.2): stores from
// search, fees (pricing), ratings (already on the doc). Money is integer minor
// units + ISO currency (02 §1).
type homeFeed struct {
	Location   geoPoint   `json:"location"`
	Feed       []feedItem `json:"feed"`
	NextCursor *string    `json:"next_cursor"`
}

type geoPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type feedItem struct {
	StoreID     string `json:"store_id"`
	Name        string `json:"name"`
	Rating      float64 `json:"rating"`
	DistanceM   int    `json:"distance_m"`
	Open        bool   `json:"open"`
	DeliveryFee money  `json:"delivery_fee"`
	ETAMinutes  int    `json:"eta_minutes"`
}

func (s *queryServer) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	lat, lng, ok := latLng(w, r, s)
	if !ok {
		return
	}
	// Browse feed = nearby OPEN stores from search, enriched with a delivery fee
	// (pricing-promo) + ETA. In production the fee comes from pricing-promo and the
	// rating from the rating service; both are contract stubs in E2E, so the fee is
	// derived deterministically from distance here (disclosed) while rating comes
	// from the indexed rating.updated projection.
	hits := s.node.Engine.Search(index.Query{Lat: lat, Lng: lng, OpenB: true, Limit: 30})
	feed := make([]feedItem, 0, len(hits))
	for _, h := range hits {
		feed = append(feed, feedItem{
			StoreID: h.StoreID, Name: h.Name, Rating: h.Rating, DistanceM: h.DistanceM, Open: h.Open,
			DeliveryFee: deliveryFee(h.DistanceM),
			ETAMinutes:  etaMinutes(h.DistanceM),
		})
	}
	writeJSON(w, http.StatusOK, homeFeed{Location: geoPoint{Lat: lat, Lng: lng}, Feed: feed})
}

// deliveryFee is a deterministic distance-based fee (base 1500 minor + 300/km),
// standing in for the pricing-promo quote in E2E.
func deliveryFee(distanceM int) money {
	fee := int64(1500) + int64(distanceM)/1000*300
	return money{Amount: fee, Currency: "THB"}
}

func etaMinutes(distanceM int) int { return 15 + distanceM/1000*3 }

func (s *queryServer) handlePublishEvent(w http.ResponseWriter, r *http.Request) {
	var env eventbus.Envelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "body must be a valid event envelope"))
		return
	}
	if env.EventType == "" || env.Aggregate.ID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "envelope needs event_type + aggregate.id"))
		return
	}
	salt := index.SaltForDoc(env.EventID)
	if err := s.node.Publish(r.Context(), env.EventType, env, salt); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"published": true, "topic": env.EventType, "salt": salt})
}

func (s *queryServer) handleIngestDoc(w http.ResponseWriter, r *http.Request) {
	var doc index.MerchantDoc
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&doc); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "body must be a valid search document"))
		return
	}
	if doc.MerchantID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "merchant_id required"))
		return
	}
	if doc.EventAt.IsZero() {
		doc.EventAt = time.Now().UTC()
	}
	if doc.MenuVersion == 0 {
		doc.MenuVersion = 1
	}
	s.node.Engine.IndexMerchant(doc)
	writeJSON(w, http.StatusAccepted, map[string]any{"indexed": doc.MerchantID})
}

func (s *queryServer) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"docs":            s.node.Engine.DocCount(),
		"shards":          index.NumShards,
		"shard_histogram": s.node.Engine.ShardHistogram(),
		"freshness_p99":   s.node.Engine.FreshnessP99().String(),
	})
}

func latLng(w http.ResponseWriter, r *http.Request, s *queryServer) (float64, float64, bool) {
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

func (s *queryServer) only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		h(w, r)
	}
}

func (s *queryServer) fail(w http.ResponseWriter, r *http.Request, err error) {
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
