// Command placeholder is a minimal std-lib HTTP service used as the empty-stack
// stand-in for S-T1. It owns no business logic; it exists so `make up` can boot
// a healthy stack (gateway + one downstream) before any real service lands.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
)

func main() {
	port := envOr("PORT", "8081")
	name := envOr("SERVICE_NAME", "placeholder")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1 (for container healthchecks)")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": name})
	})
	// Root echoes identity so a proxied /placeholder/ hit is observable end-to-end.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": name, "path": r.URL.Path})
	})

	addr := ":" + port
	log.Printf("placeholder service %q listening on %s", name, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("placeholder server exited: %v", err)
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
