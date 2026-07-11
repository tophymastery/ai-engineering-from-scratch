package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// webhookSink records webhook bodies in receipt order (== dispatch order).
type webhookSink struct {
	mu   sync.Mutex
	recv []map[string]any
}

func (s *webhookSink) handler(w http.ResponseWriter, r *http.Request) {
	var ev map[string]any
	_ = json.NewDecoder(r.Body).Decode(&ev)
	s.mu.Lock()
	s.recv = append(s.recv, ev)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *webhookSink) snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.recv...)
}

// runScenario drives one full authorize/decline/timeout/capture/refund sequence
// against a fresh seeded PSP and returns a canonical outcome blob: sync results
// (statuses + ids) + the ordered webhook stream + the settlement CSV. For a
// fixed seed this blob must be byte-identical on every run.
func runScenario(t *testing.T, seed int64) []byte {
	t.Helper()
	sink := &webhookSink{}
	sinkSrv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer sinkSrv.Close()

	psp := NewPSP(Config{Seed: seed, Timeout: time.Millisecond})
	srv := httptest.NewServer(NewMux(psp))
	defer srv.Close()

	type step struct {
		Op     string         `json:"op"`
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	}
	var steps []step

	post := func(op, path string, in map[string]any) map[string]any {
		b, _ := json.Marshal(in)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		steps = append(steps, step{Op: op, Status: resp.StatusCode, Body: body})
		return body
	}

	amount := map[string]any{"amount": 42550, "currency": "THB"}

	// 1. good card authorizes (+ webhook). 2. ...0002 declines. 3. ...0044 times out.
	good := post("authorize_ok", "/v1/psp/authorize", map[string]any{
		"card_number": "4111111111111111", "amount": amount,
		"order_ref": "ord_x", "callback_url": sinkSrv.URL,
	})
	post("authorize_decline", "/v1/psp/authorize", map[string]any{"card_number": "4000000000000002", "amount": amount})
	post("authorize_timeout", "/v1/psp/authorize", map[string]any{"card_number": "4000000000000044", "amount": amount})

	// 4. capture the good auth (+ webhook). 5. refund it (+ webhook).
	cap := post("capture", "/v1/psp/capture", map[string]any{
		"auth_id": good["auth_id"], "amount": amount,
		"settlement_date": "2026-07-11", "callback_url": sinkSrv.URL,
	})
	post("refund", "/v1/psp/refund", map[string]any{
		"capture_id": cap["capture_id"], "amount": amount, "callback_url": sinkSrv.URL,
	})

	// Wait for all three webhooks to land (deterministic FIFO order).
	deadline := time.Now().Add(2 * time.Second)
	for len(sink.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	psp.Close()

	// Settlement CSV for the day of the capture.
	sresp, err := http.Get(srv.URL + "/v1/psp/settlement-file?date=2026-07-11")
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	csv, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()

	out := map[string]any{
		"steps":      steps,
		"webhooks":   sink.snapshot(),
		"settlement": string(csv),
	}
	blob, _ := json.Marshal(out)
	return blob
}

// TestDeterministic50Reruns is the S-T7 test criterion: card ...0002 declines,
// ...0044 times out, webhooks fire, and the ENTIRE outcome sequence is 100%
// deterministic across 50 seeded reruns. Run with -race to prove the webhook
// path is free of data races.
func TestDeterministic50Reruns(t *testing.T) {
	const runs = 50
	first := runScenario(t, 42)

	// Assert the scripted outcomes are what the card script promises.
	var decoded struct {
		Steps    []struct{ Op string; Status int } `json:"steps"`
		Webhooks []struct {
			EventType string `json:"event_type"`
		} `json:"webhooks"`
		Settlement string `json:"settlement"`
	}
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"authorize_ok": 200, "authorize_decline": 402, "authorize_timeout": 504, "capture": 200, "refund": 200}
	for _, s := range decoded.Steps {
		if want[s.Op] != s.Status {
			t.Fatalf("op %s: got status %d want %d", s.Op, s.Status, want[s.Op])
		}
	}
	if len(decoded.Webhooks) != 3 {
		t.Fatalf("want 3 webhooks, got %d", len(decoded.Webhooks))
	}
	wantEv := []string{"payment.authorized", "payment.captured", "payment.refunded"}
	for i, ev := range decoded.Webhooks {
		if ev.EventType != wantEv[i] {
			t.Fatalf("webhook[%d]=%s want %s (ordering not deterministic)", i, ev.EventType, wantEv[i])
		}
	}
	if !bytes.Contains([]byte(decoded.Settlement), []byte("capture_id,auth_id,amount_minor")) {
		t.Fatalf("settlement CSV header missing: %q", decoded.Settlement)
	}

	// Byte-identical across all 50 reruns.
	for i := 1; i < runs; i++ {
		got := runScenario(t, 42)
		if !bytes.Equal(first, got) {
			t.Fatalf("run %d diverged from run 0 (non-deterministic)\nfirst=%s\n got=%s", i, first, got)
		}
	}
	t.Logf("50/50 reruns byte-identical; outcomes {decline=402,timeout=504}; webhooks ordered authorized->captured->refunded")
}

// TestDifferentSeedDiffers guards that the seed actually drives the RNG (a
// constant-output bug would also pass the determinism test).
func TestDifferentSeedDiffers(t *testing.T) {
	a := runScenario(t, 1)
	b := runScenario(t, 2)
	if bytes.Equal(a, b) {
		t.Fatal("different seeds produced identical output — RNG not seed-driven")
	}
}
