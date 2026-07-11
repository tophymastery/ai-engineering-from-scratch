// Command payment-sim is the S-T7 scriptable PSP fake (03 §5). It authorizes,
// captures and refunds cards, fires deterministic ordered webhooks, and serves a
// per-day settlement CSV — all driven by a seeded RNG so behaviour is
// byte-identical across reruns for the same seed. Std-lib only, zero config
// beyond env vars, so it boots in compose and the process-mode dev stack alike.
//
// Endpoints (02 §1 canonical /v1 paths; bare aliases kept for task-spec DX):
//
//	POST /v1/psp/authorize        (alias /psp/authorize)
//	POST /v1/psp/capture          (alias /psp/capture)
//	POST /v1/psp/refund           (alias /psp/refund)
//	GET  /v1/psp/settlement-file  (alias /psp/settlement-file) ?date=YYYY-MM-DD
//	GET  /healthz
//
// Env: PORT (8091), PSP_SEED (42), PSP_TIMEOUT_MS (200 — the "...0044" sleep).
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	port := envOr("PORT", "8091")
	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1 (container healthcheck)")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	seed := int64(envInt("PSP_SEED", 42))
	timeout := time.Duration(envInt("PSP_TIMEOUT_MS", 200)) * time.Millisecond
	psp := NewPSP(Config{Seed: seed, Timeout: timeout})
	defer psp.Close()

	mux := NewMux(psp)
	addr := ":" + port
	log.Printf("payment-sim on %s (seed=%d timeout=%s)", addr, seed, timeout)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("payment-sim exited: %v", err)
	}
}

// NewMux wires the HTTP surface for a PSP (exported so tests share it).
func NewMux(psp *PSP) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "payment-sim"})
	})

	authorize := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CardNumber  string `json:"card_number"`
			Amount      Money  `json:"amount"`
			OrderRef    string `json:"order_ref"`
			CallbackURL string `json:"callback_url"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.CardNumber == "" {
			writeErr(w, r, &apiErr{http.StatusBadRequest, "VALIDATION", "card_number is required", "card_number", "required", false})
			return
		}
		body, e := psp.Authorize(in.CardNumber, in.Amount, in.OrderRef, in.CallbackURL)
		respond(w, r, body, e)
	}
	capture := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AuthID         string `json:"auth_id"`
			Amount         Money  `json:"amount"`
			SettlementDate string `json:"settlement_date"`
			CallbackURL    string `json:"callback_url"`
		}
		if !decode(w, r, &in) {
			return
		}
		body, e := psp.Capture(in.AuthID, in.Amount, in.SettlementDate, in.CallbackURL)
		respond(w, r, body, e)
	}
	refund := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CaptureID   string `json:"capture_id"`
			Amount      Money  `json:"amount"`
			CallbackURL string `json:"callback_url"`
		}
		if !decode(w, r, &in) {
			return
		}
		body, e := psp.Refund(in.CaptureID, in.Amount, in.CallbackURL)
		respond(w, r, body, e)
	}
	settlement := func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Query().Get("date")
		if date == "" {
			writeErr(w, r, &apiErr{http.StatusBadRequest, "VALIDATION", "date is required (YYYY-MM-DD)", "date", "required", false})
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, psp.SettlementFile(date))
	}

	// Canonical /v1 paths + bare task-spec aliases route to the same handlers.
	for _, base := range []string{"/v1/psp", "/psp"} {
		mux.HandleFunc(base+"/authorize", postOnly(authorize))
		mux.HandleFunc(base+"/capture", postOnly(capture))
		mux.HandleFunc(base+"/refund", postOnly(refund))
		mux.HandleFunc(base+"/settlement-file", getOnly(settlement))
	}
	return mux
}

func respond(w http.ResponseWriter, r *http.Request, body any, e *apiErr) {
	if e != nil {
		writeErr(w, r, e)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func postOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, r, &apiErr{http.StatusMethodNotAllowed, "VALIDATION", "method not allowed", "", "", false})
			return
		}
		h(w, r)
	}
}

func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, r, &apiErr{http.StatusMethodNotAllowed, "VALIDATION", "method not allowed", "", "", false})
			return
		}
		h(w, r)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, r, &apiErr{http.StatusBadRequest, "VALIDATION", "request body must be valid JSON", "body", "invalid_json", false})
		return false
	}
	return true
}

// writeErr emits the 02 §2 error envelope. trace_id is echoed from the ingress
// header when present so a fake response threads into the caller's trace.
func writeErr(w http.ResponseWriter, r *http.Request, e *apiErr) {
	env := map[string]any{"error": map[string]any{
		"code":      e.code,
		"message":   e.message,
		"trace_id":  traceID(r),
		"retryable": e.retryable,
	}}
	if e.field != "" {
		env["error"].(map[string]any)["details"] = []map[string]string{{"field": e.field, "reason": e.reason}}
	}
	writeJSON(w, e.status, env)
}

func traceID(r *http.Request) string {
	if tp := r.Header.Get("traceparent"); len(tp) >= 35 {
		return tp[3:35] // trace-id segment of a W3C traceparent
	}
	return "00000000000000000000payment-sim0"[:32]
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
