// Command placeholder is the empty-stack stand-in service. For S-T2 it grows
// two test-only affordances used by the preview + prod-safety harness:
//
//   - a tenant-scoped in-memory KV echo store (/kv) keyed by the
//     X-Preview-Tenant header — proves cross-PR preview isolation (zero bleed).
//   - a header-echo endpoint (/headers) — lets the gateway strip test assert a
//     backdoor header never reached upstream.
//
// It also mounts libs/testhooks middleware (build-tag guarded) so the
// backdoor-scan covers services, not just the gateway.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/shop-platform/shop/libs/testhooks"
)

const tenantHeader = "X-Preview-Tenant"

// tenantStore is a per-tenant KV map: tenant -> (key -> value). Each preview
// tenant (pr-<n>) sees only its own namespace, so two PRs mutating the same
// entity type cannot observe each other's writes.
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": name})
	})

	// /kv?key=K[&value=V] — tenant-scoped echo store. With value: set+return.
	// Without value: read. Tenant comes from X-Preview-Tenant (default "base").
	mux.HandleFunc("/kv", func(w http.ResponseWriter, r *http.Request) {
		tenant := tenantOr(r, "base")
		key := r.URL.Query().Get("key")
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing key"})
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
	// The gateway strip test asserts these are empty when sent through a
	// prod-mode gateway.
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

	// testhooks middleware: active only in a `-tags testhooks` build.
	handler := testhooks.Middleware(mux)

	addr := ":" + port
	log.Printf("placeholder service %q listening on %s (testhooks_compiled=%v)", name, addr, testhooks.Enabled)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("placeholder server exited: %v", err)
	}
}

func tenantOr(r *http.Request, def string) string {
	if v := r.Header.Get(tenantHeader); v != "" {
		return v
	}
	return def
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
