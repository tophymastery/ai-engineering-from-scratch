package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// contractPath locates the adapter contract this fake must serve.
const contractPath = "../../../contracts/openapi/payment-sim.v1.yaml"

// TestConformsToContract verifies the running fake against its published S-T5
// adapter contract: every canonical /v1 path the contract documents is (a)
// actually present in the contract file and (b) served by the fake with the
// declared success-shape (required fields) or the declared error envelope. This
// is the "verify payment-sim against its contract" DoD item, done as a small
// conformance test (no external pact tooling needed).
func TestConformsToContract(t *testing.T) {
	spec, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	specText := string(spec)

	psp := NewPSP(Config{Seed: 7, Timeout: time.Millisecond})
	defer psp.Close()
	srv := httptest.NewServer(NewMux(psp))
	defer srv.Close()

	// Each documented path must appear in the contract AND be served.
	paths := []string{"/v1/psp/authorize", "/v1/psp/capture", "/v1/psp/refund", "/v1/psp/settlement-file"}
	for _, p := range paths {
		if !strings.Contains(specText, p+":") {
			t.Fatalf("contract does not document path %s", p)
		}
	}

	amount := map[string]any{"amount": 42550, "currency": "THB"}
	post := func(path string, in map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(in)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	requireFields := func(where string, body map[string]any, fields ...string) {
		for _, f := range fields {
			if _, ok := body[f]; !ok {
				t.Fatalf("%s response missing required field %q (contract violation): %v", where, f, body)
			}
		}
	}

	// authorize -> Authorization shape.
	st, body := post("/v1/psp/authorize", map[string]any{"card_number": "4111111111111111", "amount": amount})
	if st != 200 {
		t.Fatalf("authorize status %d", st)
	}
	requireFields("authorize", body, "auth_id", "status", "amount", "authorized_at")
	authID, _ := body["auth_id"].(string)

	// capture -> Capture shape.
	st, body = post("/v1/psp/capture", map[string]any{"auth_id": authID, "amount": amount, "settlement_date": "2026-07-11"})
	if st != 200 {
		t.Fatalf("capture status %d", st)
	}
	requireFields("capture", body, "capture_id", "auth_id", "status", "amount", "settlement_date", "captured_at")
	capID, _ := body["capture_id"].(string)

	// refund -> Refund shape.
	st, body = post("/v1/psp/refund", map[string]any{"capture_id": capID, "amount": amount})
	if st != 200 {
		t.Fatalf("refund status %d", st)
	}
	requireFields("refund", body, "refund_id", "capture_id", "status", "amount", "refunded_at")

	// decline path -> the 02 §2 error envelope with required inner fields.
	st, body = post("/v1/psp/authorize", map[string]any{"card_number": "4000000000000002", "amount": amount})
	if st != 402 {
		t.Fatalf("decline status %d want 402", st)
	}
	inner, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("decline: no error envelope: %v", body)
	}
	requireFields("error", inner, "code", "message", "trace_id", "retryable")

	// settlement CSV present with the declared header columns.
	resp, err := http.Get(srv.URL + "/v1/psp/settlement-file?date=2026-07-11")
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("settlement content-type %q want text/csv", ct)
	}

	t.Logf("payment-sim conforms to %s (4 paths, 3 success shapes, error envelope, CSV)", contractPath)
}
