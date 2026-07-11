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
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/shop-platform/shop/libs/testhooks"
)

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

	routes := []route{
		{Prefix: "/placeholder/", Upstream: placeholderURL},
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

	// Middleware chain (outermost first):
	//   stripBackdoors(mode) -> testhooks.Middleware -> mux
	// Strip runs first so that even a mis-built (testhooks-tagged) binary
	// running in prod mode has the headers removed before anything reads them.
	var handler http.Handler = mux
	handler = testhooks.Middleware(handler)
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
