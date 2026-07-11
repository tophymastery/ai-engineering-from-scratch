// Command gateway is the edge for the shop platform. On top of the S-T1
// std-lib reverse proxy it adds the D29 (S-T2) prod-safety layers:
//
//   - Layer 2 (unconditional strip): when GATEWAY_MODE=prod the gateway strips
//     the X-Test-Clock / X-Flag-Override backdoor headers from every inbound
//     request BEFORE proxying upstream — no matter how the binary was built.
//   - Layer 3 (prod-log alert): if either header is seen in prod, the gateway
//     emits a WARN log line (04 §3 envelope, code TESTHOOK_HEADER_STRIPPED)
//     immediately — the alert source.
//   - Layer 1 (compiled-out) lives in libs/testhooks and is wired here via the
//     `testhooks` build tag; a prod build gets a no-op middleware.
//
// Authn, request_id minting, and rate limiting land in later tasks.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/testhooks"
)

// bffsWithAuthPassthrough are the BFF prefixes whose /v1/auth/* paths the
// gateway routes DIRECTLY to identity-auth (V-T1 req 3). The E2E BFF slots are
// contract stubs today; a BFF slice will own richer auth flows later, at which
// point it fronts identity itself and these passthroughs are removed. Documented
// in contracts/openapi/{customer,driver}-bff.v1.yaml.
var bffsWithAuthPassthrough = []string{"customer-bff", "driver-bff"}

// bffsWithProfilePassthrough are the BFF prefixes whose /v1/profiles* + /v1/tokens/*
// paths the gateway routes DIRECTLY to identity-profile (V-T2 / D3 — profile CRUD
// + erasure via customer-bff). Same lifecycle as the auth passthrough above.
var bffsWithProfilePassthrough = []string{"customer-bff"}

// bffsWithCatalogPassthrough are the BFF prefixes whose /v1/merchants* paths the
// gateway routes DIRECTLY to merchant-catalog (V-T3 — menu editor + store-status
// via merchant-bff, under ETag/If-Match). Same discover-from-route-table
// lifecycle as the passthroughs above; documented in
// contracts/openapi/merchant-bff.v1.yaml.
var bffsWithCatalogPassthrough = []string{"merchant-bff"}

// bffsWithBrowsePassthrough are the BFF prefixes whose browse + geo-search paths
// the gateway routes DIRECTLY to the search-query slot (V-T4 — GET
// /v1/customer/home + /v1/search via customer-bff, behind search_v2). Same
// discover-from-route-table lifecycle; documented in
// contracts/openapi/customer-bff.v1.yaml + search.v1.yaml.
var bffsWithBrowsePassthrough = []string{"customer-bff"}

// backdoorHeaders are the D29 test backdoors. They are stripped unconditionally
// at the edge in prod mode. Listing them here (for stripping) is NOT a leak of
// the backdoor itself: the strip path contains no handler and no testhooks
// marker, so the backdoor-scan stays clean on a prod build.
var backdoorHeaders = []string{"X-Test-Clock", "X-Flag-Override"}

// route maps an inbound path prefix to an upstream base URL.
type route struct {
	Prefix   string
	Upstream string
}

func main() {
	port := envOr("PORT", "8080")
	placeholderURL := envOr("PLACEHOLDER_URL", "http://localhost:8081")
	mode := envOr("GATEWAY_MODE", "dev") // dev | preview | staging | prod

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1 (for container healthchecks)")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	// Route table. By default (S-T1) the edge fronts only the placeholder. In the
	// shared E2E env (S-T8) tools/e2e-up.sh writes a GATEWAY_ROUTES file mapping a
	// prefix to every service+BFF+fake slot, so the same gateway binary fans out
	// to the whole topology with zero code change — and keeps routing across a
	// stub->real swap because the upstream port is stable per slot.
	routes := []route{
		{Prefix: "/placeholder/", Upstream: placeholderURL},
	}
	if rf := os.Getenv("GATEWAY_ROUTES"); rf != "" {
		loaded, err := loadRoutes(rf)
		if err != nil {
			log.Fatalf("gateway: GATEWAY_ROUTES=%q: %v", rf, err)
		}
		routes = loaded
		log.Printf("gateway: loaded %d route(s) from %s", len(routes), rf)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "gateway", "mode": mode})
	})
	mux.HandleFunc("/routes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, routes)
	})

	for _, rt := range routes {
		target, err := url.Parse(rt.Upstream)
		if err != nil {
			log.Fatalf("bad upstream %q for %q: %v", rt.Upstream, rt.Prefix, err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		prefix := rt.Prefix
		mux.Handle(prefix, http.StripPrefix(strings.TrimRight(prefix, "/"), proxy))
		log.Printf("route %s -> %s", prefix, rt.Upstream)
	}

	// --- V-T1 / D4 edge auth wiring ---
	// identity-auth is the JWKS + denylist source; discover its upstream from the
	// route table so no extra config is needed (stable per-slot port survives the
	// stub->real swap). When present, register the BFF /v1/auth/* passthroughs.
	identityBase := upstreamFor(routes, "/identity/")
	for _, bff := range bffsWithAuthPassthrough {
		bffPrefix := "/" + bff + "/"
		if identityBase == "" || upstreamFor(routes, bffPrefix) == "" {
			continue
		}
		idURL, err := url.Parse(identityBase)
		if err != nil {
			log.Fatalf("bad identity upstream %q: %v", identityBase, err)
		}
		// A more specific pattern than the generic BFF route: ServeMux longest-
		// prefix match sends /{bff}/v1/auth/* here, everything else to the stub.
		authProxy := httputil.NewSingleHostReverseProxy(idURL)
		mux.Handle(bffPrefix+"v1/auth/", http.StripPrefix("/"+bff, authProxy))
		log.Printf("auth passthrough %sv1/auth/* -> %s (identity-auth)", bffPrefix, identityBase)
	}

	// --- V-T2 / D3 profile passthrough ---
	// identity-profile OWNS PII; the customer app does profile CRUD + erasure
	// through the BFF. Same discover-from-route-table pattern as the auth
	// passthrough: /customer-bff/v1/profiles* and /v1/tokens/* route directly to
	// the identity-profile slot (stable per-slot port survives the stub->real
	// swap). Documented in contracts/openapi/customer-bff.v1.yaml.
	profileBase := upstreamFor(routes, "/identity-profile/")
	for _, bff := range bffsWithProfilePassthrough {
		bffPrefix := "/" + bff + "/"
		if profileBase == "" || upstreamFor(routes, bffPrefix) == "" {
			continue
		}
		pURL, err := url.Parse(profileBase)
		if err != nil {
			log.Fatalf("bad identity-profile upstream %q: %v", profileBase, err)
		}
		pProxy := httputil.NewSingleHostReverseProxy(pURL)
		strip := http.StripPrefix("/"+bff, pProxy)
		// Exact create path + the {user_token} subtree + token resolution.
		mux.Handle(bffPrefix+"v1/profiles", strip)
		mux.Handle(bffPrefix+"v1/profiles/", strip)
		mux.Handle(bffPrefix+"v1/tokens/", strip)
		log.Printf("profile passthrough %sv1/profiles* -> %s (identity-profile)", bffPrefix, profileBase)
	}

	// --- V-T3 merchant-catalog passthrough ---
	// merchant-catalog owns menus + store status; the merchant app edits them
	// through the BFF. Same discover-from-route-table pattern:
	// /merchant-bff/v1/merchants* routes directly to the merchant-catalog slot
	// (stable per-slot port survives the stub->real swap). ETag/If-Match headers
	// pass through the reverse proxy untouched (02 §1 concurrency preserved).
	catalogBase := upstreamFor(routes, "/merchant-catalog/")
	for _, bff := range bffsWithCatalogPassthrough {
		bffPrefix := "/" + bff + "/"
		if catalogBase == "" || upstreamFor(routes, bffPrefix) == "" {
			continue
		}
		cURL, err := url.Parse(catalogBase)
		if err != nil {
			log.Fatalf("bad merchant-catalog upstream %q: %v", catalogBase, err)
		}
		cProxy := httputil.NewSingleHostReverseProxy(cURL)
		strip := http.StripPrefix("/"+bff, cProxy)
		mux.Handle(bffPrefix+"v1/merchants", strip)
		mux.Handle(bffPrefix+"v1/merchants/", strip)
		log.Printf("catalog passthrough %sv1/merchants* -> %s (merchant-catalog)", bffPrefix, catalogBase)
	}

	// --- V-T4 / V-T5 browse passthrough ---
	// The customer browse feed + geo search are reached through the BFF. Same
	// discover-from-route-table pattern (stable per-slot port survives the stub->real
	// swap):
	//   /customer-bff/v1/search         -> search-query (V-T4 geo search)
	//   /customer-bff/v1/customer/home  -> ranking if present (V-T5 re-ranker),
	//                                      else search-query (V-T4 static browse)
	// D17 is two-phase: search RETRIEVES the top-500, ranking RE-RANKS to the top-50
	// (ranking is a client of the search browse contract, fed via SEARCH_URL). When
	// no ranking slot exists the feed falls back to the search browse feed unchanged.
	searchBase := upstreamFor(routes, "/search/")
	rankingBase := upstreamFor(routes, "/ranking/")
	browseHomeBase := rankingBase
	if browseHomeBase == "" {
		browseHomeBase = searchBase
	}
	for _, bff := range bffsWithBrowsePassthrough {
		bffPrefix := "/" + bff + "/"
		if upstreamFor(routes, bffPrefix) == "" {
			continue
		}
		if searchBase != "" {
			sURL, err := url.Parse(searchBase)
			if err != nil {
				log.Fatalf("bad search upstream %q: %v", searchBase, err)
			}
			mux.Handle(bffPrefix+"v1/search", http.StripPrefix("/"+bff, httputil.NewSingleHostReverseProxy(sURL)))
		}
		if browseHomeBase != "" {
			hURL, err := url.Parse(browseHomeBase)
			if err != nil {
				log.Fatalf("bad browse upstream %q: %v", browseHomeBase, err)
			}
			mux.Handle(bffPrefix+"v1/customer/home", http.StripPrefix("/"+bff, httputil.NewSingleHostReverseProxy(hURL)))
		}
		who := "search"
		if rankingBase != "" {
			who = "ranking (re-rank) -> search (retrieval)"
		}
		log.Printf("browse passthrough %sv1/customer/home -> %s [%s]; v1/search -> %s (search)", bffPrefix, browseHomeBase, who, searchBase)
	}

	flagSet := flags.FromEnv()
	authEnabled := flagSet.Bool("auth_jwt_edge", false)
	pollEvery := envDuration("DENYLIST_POLL", 5*time.Second)
	auth := newEdgeAuth(authEnabled && identityBase != "", identityBase, pollEvery)
	auth.start(context.Background())
	log.Printf("edge auth: enabled=%v identity=%q denylist_poll=%s", auth.enabled, identityBase, pollEvery)

	// Middleware chain (outermost first):
	//   stripBackdoors(mode) -> auth.middleware -> testhooks.Middleware -> mux
	// Strip runs first so that even a mis-built (testhooks-tagged) binary
	// running in prod mode has the headers removed before anything reads them.
	// Auth runs next: it strips spoofed identity headers ALWAYS and verifies a
	// presented bearer token locally (D4 — no call to identity on the hot path).
	var handler http.Handler = mux
	handler = testhooks.Middleware(handler)
	handler = auth.middleware(handler)
	handler = stripBackdoors(mode, handler)

	addr := ":" + port
	log.Printf("gateway listening on %s (mode=%s, testhooks_compiled=%v)", addr, mode, testhooks.Enabled)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("gateway server exited: %v", err)
	}
}

// stripBackdoors removes the D29 backdoor headers from inbound requests when
// running in prod mode, and emits the WARN alert line if any were present.
// Outside prod mode it is a passthrough (dev/preview/staging legitimately use
// the headers with a testhooks build).
func stripBackdoors(mode string, next http.Handler) http.Handler {
	if mode != "prod" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range backdoorHeaders {
			if v := r.Header.Get(h); v != "" {
				r.Header.Del(h)
				alert(mode, r, h, v)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// alert emits the prod-log alert line (04 §3 envelope, level WARN). An alerting
// rule keys on error.code == "TESTHOOK_HEADER_STRIPPED".
func alert(mode string, r *http.Request, header, value string) {
	line := map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		"level":     "WARN",
		"service":   "gateway",
		"env":       mode,
		"direction": "ingress",
		"protocol":  "http",
		"route":     r.Method + " " + r.URL.Path,
		"peer":      r.RemoteAddr,
		"msg":       "test backdoor header seen in prod; stripped at edge",
		"error": map[string]any{
			"code":      "TESTHOOK_HEADER_STRIPPED",
			"retryable": false,
			"header":    header,
			"value_len": len(value),
		},
	}
	b, _ := json.Marshal(line)
	// stdout only (04 §3): the log pipeline ships it; the alert fires off this line.
	log.Println(string(b))
}

// loadRoutes reads a JSON array of {"prefix","upstream"} objects (written by
// tools/e2e-up.sh from deploy/e2e/topology.yaml + the runtime overlay).
func loadRoutes(path string) ([]route, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var routes []route
	if err := json.Unmarshal(b, &routes); err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("no routes in file")
	}
	for _, rt := range routes {
		if rt.Prefix == "" || rt.Upstream == "" {
			return nil, fmt.Errorf("route with empty prefix/upstream: %+v", rt)
		}
	}
	return routes, nil
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

// upstreamFor returns the upstream URL registered for an exact route prefix.
func upstreamFor(routes []route, prefix string) string {
	for _, rt := range routes {
		if rt.Prefix == prefix {
			return rt.Upstream
		}
	}
	return ""
}

// envDuration reads a Go duration (e.g. "5s") from an env var, else def.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
