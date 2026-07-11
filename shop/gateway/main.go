// Command gateway is the minimal edge for S-T1: a std-lib reverse proxy with a
// static route table. It exposes /healthz for its own liveness and proxies
// /placeholder/* to the placeholder service. Real gateway concerns (authn,
// request_id minting, rate limiting — docs 01 §2, 04 §3) land in later tasks;
// this is the scaffold those hang off of.
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
)

// route maps an inbound path prefix to an upstream base URL. In S-T1 the table
// has a single entry; new services register here (or, later, via contracts).
type route struct {
	Prefix  string
	Upstream string
}

func main() {
	port := envOr("PORT", "8080")
	placeholderURL := envOr("PLACEHOLDER_URL", "http://localhost:8081")

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
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "gateway"})
	})
	// Expose the route table so the topology is inspectable in the empty stack.
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

	addr := ":" + port
	log.Printf("gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("gateway server exited: %v", err)
	}
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
