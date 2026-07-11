// Command placeholder is the empty-stack stand-in service and the S-T3
// REFERENCE SERVICE that exercises all five shared libs end-to-end:
//
//   - libs/otel     — ingress traceparent continuation; trace_id in every log
//   - libs/logging  — the 04 §3 envelope from shared middleware (ingress),
//                     read paths sampled, mutations/errors always logged
//   - libs/errors   — the 02 §2 error envelope + registry HTTP mapping
//   - libs/flags    — env-backed flags + per-request X-Flag-Override (non-prod)
//   - libs/idempotency — D9 effect-once on POST /kv (Idempotency-Key required),
//                     durable via the caller's own transaction; MemStore stands
//                     in for a service's PG here so the reference needs no DB.
//
// It also keeps the S-T2 affordances: the tenant-scoped GET /kv echo store
// (preview isolation), /headers (gateway strip test), and the testhooks
// middleware (build-tag guarded backdoor scan).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/idempotency"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

const tenantHeader = "X-Preview-Tenant"

// tenantStore is a per-tenant KV map: tenant -> (key -> value).
type tenantStore struct {
	mu   sync.RWMutex
	data map[string]map[string]string
}

func newTenantStore() *tenantStore {
	return &tenantStore{data: map[string]map[string]string{}}
}

func (s *tenantStore) set(tenant, key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[tenant] == nil {
		s.data[tenant] = map[string]string{}
	}
	s.data[tenant][key] = val
}

func (s *tenantStore) get(tenant, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[tenant][key]
	return v, ok
}

func main() {
	port := envOr("PORT", "8081")
	name := envOr("SERVICE_NAME", "placeholder")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1 (for container healthchecks)")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	store := newTenantStore()

	// --- shared libs bootstrap (S-T3) ---
	logger := logging.New(logging.Config{
		Service:    name,
		Version:    envOr("SERVICE_VERSION", "0.0.0-dev"),
		Env:        envOr("ENV", "dev"),
		Region:     envOr("REGION", "local"),
		SampleRate: 1.0, // full logging in dev; prod fleet sets 1–5% on read paths (D27)
	})
	flagSet := flags.FromEnv()
	// D9 idempotency: MemStore stands in for the service's own PG; MemCache
	// stands in for Redis (advisory). A real slice swaps NewSQLStore(db, pg) +
	// a Redis-backed cache with zero call-site changes.
	idem := idempotency.New(idempotency.NewMemStore(), idempotency.NewMemCache())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":        "ok",
			"service":       name,
			"otel_exporter": otel.ExporterMode(),
			"flag_override": boolStr(flags.OverrideActive()),
		})
	})

	// POST /kv — the S-T3 idempotent MUTATION. Requires Idempotency-Key (02 §3);
	// the write runs exactly once inside the durable transaction (D9), wrapped in
	// logging + otel + errors + flags. GET /kv keeps the S-T2 echo behaviour.
	mux.HandleFunc("/kv", func(w http.ResponseWriter, r *http.Request) {
		tenant := tenantOr(r, "base")
		if r.Method == http.MethodPost {
			// Feature-gate the mutation; per-request override honoured in non-prod.
			if !flagSet.BoolCtx(r.Context(), "kv_v1", true) {
				shoperr.WriteRequest(w, r, shoperr.New(shoperr.CodeForbidden, "kv_v1 is disabled"), logging.TraceIDFromRequest)
				return
			}
			idem.HTTP(w, r, logging.TraceIDFromRequest, func(ctx context.Context, tx idempotency.Execer, body []byte) (int, []byte, error) {
				var in struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				}
				if err := json.Unmarshal(body, &in); err != nil || in.Key == "" {
					return 0, nil, shoperr.New(shoperr.CodeValidation, `body must be {"key":..,"value":..}`,
						shoperr.Detail{Field: "key", Reason: "required"})
				}
				store.set(tenant, in.Key, in.Value) // the effect — runs exactly once per key
				resp, _ := json.Marshal(map[string]any{"tenant": tenant, "key": in.Key, "value": in.Value, "op": "set"})
				return http.StatusCreated, resp, nil
			})
			return
		}

		// GET /kv?key=K[&value=V] — S-T2 tenant echo store (preview isolation).
		key := r.URL.Query().Get("key")
		if key == "" {
			shoperr.WriteRequest(w, r, shoperr.New(shoperr.CodeValidation, "missing key",
				shoperr.Detail{Field: "key", Reason: "required"}), logging.TraceIDFromRequest)
			return
		}
		if val, hasVal := r.URL.Query()["value"]; hasVal {
			store.set(tenant, key, val[0])
			writeJSON(w, http.StatusOK, map[string]any{"tenant": tenant, "key": key, "value": val[0], "op": "set"})
			return
		}
		v, ok := store.get(tenant, key)
		writeJSON(w, http.StatusOK, map[string]any{"tenant": tenant, "key": key, "value": v, "found": ok, "op": "get"})
	})

	// /headers — echo the backdoor headers this service actually received.
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service":         name,
			"tenant":          tenantOr(r, ""),
			"X-Test-Clock":    r.Header.Get("X-Test-Clock"),
			"X-Flag-Override": r.Header.Get("X-Flag-Override"),
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": name, "path": r.URL.Path, "tenant": tenantOr(r, "")})
	})

	// Middleware chain (outer→inner): otel continues the trace so logging can
	// read trace_id; logging emits the 04 §3 ingress envelope; testhooks (build-
	// tag guarded) stashes X-Flag-Override for flags.BoolCtx in non-prod builds.
	handler := otel.Middleware(
		logger.Middleware(enrich)(
			testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("placeholder service %q on %s (testhooks=%v otel=%s)", name, addr, testhooks.Enabled, otel.ExporterMode())
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("placeholder server exited: %v", err)
	}
}

// enrich attaches the tenant as a business key (prefixed/opaque only — never PII)
// and a route pattern so the log line stays low-cardinality.
func enrich(r *http.Request, e *logging.Entry) {
	if t := r.Header.Get(tenantHeader); t != "" {
		e.Keys["tenant"] = t
	}
	e.Route = r.Method + " " + r.URL.Path
}

func tenantOr(r *http.Request, def string) string {
	if v := r.Header.Get(tenantHeader); v != "" {
		return v
	}
	return def
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
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
