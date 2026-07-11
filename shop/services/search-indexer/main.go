// Command search-indexer is one half of the V-T4 Search & browse slice (01 §1
// `search`; decisions D17 + D11). It owns the write path of the discovery read
// model: it CONSUMES the merchant fan-out events (menu.updated,
// store.status_changed, rating.updated) — salted `merchant_id#0..15` (D11), LWW
// by version — through the established inbox pattern and maintains the in-process
// inverted index + H3-res-5 shard router (index package). Rating updates are
// debounced to ≤1 index write / merchant / 5 min (D17); a bulk reindex runs on
// dedicated ingest workers with backpressure so it never contends with feed reads.
//
// In production this is a deployment separate from search-query, both over a
// shared per-cell OpenSearch. This sandbox has no OpenSearch and no cross-process
// shared store, so the in-process engine IS the store (disclosed in
// VERIFICATION.md §V-T4); the query half embeds an indexer for the demo.
//
// HTTP surface (internal ingest tier):
//
//	GET  /healthz
//	POST /v1/index/events     publish a raw event envelope onto the projection bus
//	POST /v1/index/merchants  upsert a search document directly (admin ingest/seed)
//	GET  /v1/index/stats      doc count, per-shard histogram, freshness p99
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
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

type indexerServer struct {
	node    *index.Node
	log     *logging.Logger
	flags   *flags.Set
	enabled bool
}

func main() {
	port := envOr("PORT", "8114")
	name := envOr("SERVICE_NAME", "search-indexer")

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
	srv := &indexerServer{
		node: node,
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("search_v2", false),
	}

	// Background ticker flushes debounced rating aggregates whose 5-min window has
	// elapsed (the last update in a burst is not stranded).
	go srv.ratingFlushLoop(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/index/events", srv.only(http.MethodPost, srv.handlePublishEvent))
	mux.HandleFunc("/v1/index/merchants", srv.only(http.MethodPost, srv.handleIngestDoc))
	mux.HandleFunc("/v1/index/stats", srv.only(http.MethodGet, srv.handleStats))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))
	addr := ":" + port
	log.Printf("search-indexer %q on %s (env=%s region=%s search_v2=%v)", name, addr, env, region, srv.enabled)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("search-indexer server exited: %v", err)
	}
}

func (s *indexerServer) ratingFlushLoop(ctx context.Context) {
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

func (s *indexerServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "search-indexer",
		"search_v2":     s.flags.BoolCtx(r.Context(), "search_v2", s.enabled),
		"docs":          s.node.Engine.DocCount(),
		"otel_exporter": otel.ExporterMode(),
	})
}

// handlePublishEvent accepts a raw 02 §4.3 envelope and publishes it onto the
// projection bus (salted by doc), exercising the real consumer path.
func (s *indexerServer) handlePublishEvent(w http.ResponseWriter, r *http.Request) {
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

// handleIngestDoc upserts a search document directly (admin/seed path).
func (s *indexerServer) handleIngestDoc(w http.ResponseWriter, r *http.Request) {
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

func (s *indexerServer) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"docs":            s.node.Engine.DocCount(),
		"shards":          index.NumShards,
		"shard_histogram": s.node.Engine.ShardHistogram(),
		"freshness_p99":   s.node.Engine.FreshnessP99().String(),
		"salts":           index.NumSalts,
	})
}

func (s *indexerServer) only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		h(w, r)
	}
}

func (s *indexerServer) fail(w http.ResponseWriter, r *http.Request, err error) {
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
