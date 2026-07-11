// Command ranking is the V-T5 Ranking slice (01 §1 `search`/ranking; decision
// D17: "Retrieval (top-500) is OpenSearch; a separate ranking service ML-re-ranks
// the top 50, with a static-ranking fallback flag (= shed ladder L1)"). It fronts
// the customer browse feed: it RETRIEVES the top-500 nearby stores from the search
// service (the search.v1 browse contract) and RE-RANKS them to the top-50 using an
// event-fed feature store. The gateway routes the browse BFF endpoint
// `GET /v1/customer/home` here (a passthrough, like the V-T4 search passthrough it
// supersedes for browse); geo search `/v1/search` stays on search-query.
//
// Two ranking modes, selected by the `ranking_ml` flag:
//
//	ranking_ml ON   → ML re-rank (feature-weighted model stand-in; popularity/CTR
//	                  features fed from the ranking.signal event stream)
//	ranking_ml OFF  → static ranking (retrieval order) — the fallback, which also
//	                  doubles as shed-ladder L1 (D12)
//
// Plus AUTO-FALLBACK: a health monitor probes the model; on a model outage it trips
// a breaker so the feed keeps serving the static order (availability ≥ 99.9%,
// engaged < 10 s) without any flag flip. All correctness properties (re-rank p99,
// auto-fallback timing/availability, feature-store-from-events, both flag states)
// run genuinely in-process; only infra scale is adapted (VERIFICATION.md §V-T5).
//
// HTTP surface:
//
//	GET  /healthz
//	GET  /v1/customer/home?lat=&lng=        browse feed, re-ranked top-50
//	POST /v1/rank                           re-rank a supplied candidate set (top-K)
//	POST /v1/signals/events                 ingest a ranking.signal envelope (feature store)
//	GET  /v1/rank/stats                     scorer/breaker/feature stats
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
	"github.com/shop-platform/shop/services/ranking/rank"
)

const signalGroup = "ranking-signals"

var codeRetrievalFailed = shoperr.Register("RANKING_RETRIEVAL_FAILED", 502, true, "Candidate retrieval from search failed.")

type server struct {
	node      *rank.Node
	source    rank.CandidateSource
	log       *logging.Logger
	flags     *flags.Set
	mlDefault bool
	region    string
}

func main() {
	port := envOr("PORT", "8115")
	name := envOr("SERVICE_NAME", "ranking")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	searchURL := envOr("SEARCH_URL", "http://localhost:8103")
	fs := flags.FromEnv()

	node := rank.NewNode(signalGroup, rank.Options{Clock: rank.SystemClock{}})
	srv := &server{
		node:   node,
		source: rank.NewHTTPCandidateSource(searchURL),
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:     fs,
		mlDefault: fs.Bool("ranking_ml", false),
		region:    region,
	}
	// Auto-fallback health monitor: probes the model on a fixed cadence so a model
	// outage trips the breaker (static fallback) within the detection window.
	go node.Ranker.RunHealthMonitor(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/customer/home", srv.only(http.MethodGet, srv.handleBrowse))
	mux.HandleFunc("/v1/rank", srv.only(http.MethodPost, srv.handleRank))
	mux.HandleFunc("/v1/signals/events", srv.only(http.MethodPost, srv.handleSignal))
	mux.HandleFunc("/v1/rank/stats", srv.only(http.MethodGet, srv.handleStats))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))
	addr := ":" + port
	log.Printf("ranking %q on %s (env=%s region=%s ranking_ml=%v search=%s)", name, addr, env, region, srv.mlDefault, searchURL)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("ranking server exited: %v", err)
	}
}

// mlEnabled resolves the per-request ranking_ml flag value (X-Flag-Override in
// non-prod, else the env default). This is the ML/static selector; when it is OFF
// the feed serves the static fallback (which doubles as shed-ladder L1).
func (s *server) mlEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "ranking_ml", s.mlDefault)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.node.Ranker.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "ranking",
		"ranking_ml":        s.mlEnabled(r),
		"fallback_engaged":  st.Fallback,
		"active_scorer":     st.Mode,
		"feature_merchants": st.Merchants,
		"otel_exporter":     otel.ExporterMode(),
	})
}

// homeFeed is the GET /v1/customer/home browse payload (02 §4.2). Identical shape
// to the search browse feed (V-T4) — re-ranking changes order, not fields.
type homeFeed struct {
	Location   geoPoint         `json:"location"`
	Feed       []rank.Candidate `json:"feed"`
	Ranking    rankingMeta      `json:"ranking"`
	NextCursor *string          `json:"next_cursor"`
}

// rankingMeta is an ADDITIVE object (D30) exposing which scorer produced the order
// — so the demo/e2e can assert ML-vs-static and the fallback state on the feed.
type rankingMeta struct {
	Scorer          string `json:"scorer"`           // "ml" | "static"
	MLRequested     bool   `json:"ml_requested"`     // ranking_ml flag value for this request
	FallbackEngaged bool   `json:"fallback_engaged"` // breaker open (auto-fallback)
}

type geoPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	lat, lng, ok := s.latLng(w, r)
	if !ok {
		return
	}
	mlWant := s.mlEnabled(r)
	cands, err := s.source.Candidates(r.Context(), lat, lng, 500)
	if err != nil {
		s.fail(w, r, shoperr.New(codeRetrievalFailed, err.Error()))
		return
	}
	ranked, usedML := s.node.Ranker.Rank(r.Context(), cands, rank.DefaultTopK, mlWant)
	scorer := "static"
	if usedML {
		scorer = "ml"
	}
	writeJSON(w, http.StatusOK, homeFeed{
		Location: geoPoint{Lat: lat, Lng: lng},
		Feed:     ranked,
		Ranking: rankingMeta{
			Scorer:          scorer,
			MLRequested:     mlWant,
			FallbackEngaged: s.node.Ranker.FallbackEngaged(),
		},
	})
}

// rankRequest is the POST /v1/rank body: a supplied retrieval set to re-rank. Self
// contained (no search dependency), so it is the clean pact/contract surface.
type rankRequest struct {
	Location   *geoPoint        `json:"location"`
	Candidates []rank.Candidate `json:"candidates"`
	TopK       int              `json:"top_k"`
}

type rankResponse struct {
	Results         []rank.Candidate `json:"results"`
	Scorer          string           `json:"scorer"`
	FallbackEngaged bool             `json:"fallback_engaged"`
	Count           int              `json:"count"`
}

func (s *server) handleRank(w http.ResponseWriter, r *http.Request) {
	var req rankRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "body must be a valid rank request"))
		return
	}
	k := req.TopK
	if k <= 0 {
		k = rank.DefaultTopK
	}
	ranked, usedML := s.node.Ranker.Rank(r.Context(), req.Candidates, k, s.mlEnabled(r))
	scorer := "static"
	if usedML {
		scorer = "ml"
	}
	writeJSON(w, http.StatusOK, rankResponse{
		Results:         ranked,
		Scorer:          scorer,
		FallbackEngaged: s.node.Ranker.FallbackEngaged(),
		Count:           len(ranked),
	})
}

func (s *server) handleSignal(w http.ResponseWriter, r *http.Request) {
	var env eventbus.Envelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&env); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "body must be a valid event envelope"))
		return
	}
	if env.EventType == "" || env.Aggregate.ID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "envelope needs event_type + aggregate.id"))
		return
	}
	if err := s.node.Publish(r.Context(), env); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"published": true, "topic": env.EventType})
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.node.Ranker.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"served":            st.Served,
		"served_ml":         st.ServedML,
		"served_static":     st.ServedStatic,
		"ml_errors":         st.MLErrors,
		"feature_merchants": st.Merchants,
		"fallback_engaged":  st.Fallback,
		"active_scorer":     st.Mode,
		"rerank_p99":        s.node.Ranker.ReRankP99().String(),
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
