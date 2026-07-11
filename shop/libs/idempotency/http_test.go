package idempotency

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

// Exercises the full 02 §3 wire protocol through the HTTP helper on MemStore.
func TestHTTPWireProtocol(t *testing.T) {
	m := New(NewMemStore(), NewMemCache())
	var effects int
	handler := func(w http.ResponseWriter, r *http.Request) {
		m.HTTP(w, r, nil, func(ctx context.Context, tx Execer, body []byte) (int, []byte, error) {
			effects++
			return 201, []byte(`{"stored":true}`), nil
		})
	}

	do := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/kv", strings.NewReader(body))
		if key != "" {
			req.Header.Set(KeyHeader, key)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// Missing key ⇒ 400 IDEMPOTENCY_KEY_REQUIRED.
	if r := do("", `{"a":1}`); r.Code != 400 {
		t.Fatalf("missing key: status=%d want 400", r.Code)
	} else {
		var e shoperr.Envelope
		_ = json.Unmarshal(r.Body.Bytes(), &e)
		if e.Error.Code != "IDEMPOTENCY_KEY_REQUIRED" {
			t.Fatalf("missing key code=%s", e.Error.Code)
		}
	}

	// Fresh ⇒ 201, effect runs once, not replayed.
	r1 := do("k1", `{"a":1}`)
	if r1.Code != 201 || r1.Header().Get(ReplayedHeader) == "true" {
		t.Fatalf("fresh: status=%d replayed=%q", r1.Code, r1.Header().Get(ReplayedHeader))
	}

	// Same key + same body ⇒ replay with header, effect NOT re-run.
	r2 := do("k1", `{"a":1}`)
	if r2.Code != 201 || r2.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("replay: status=%d replayed=%q", r2.Code, r2.Header().Get(ReplayedHeader))
	}
	if r2.Body.String() != r1.Body.String() {
		t.Fatalf("replay body mismatch: %q vs %q", r2.Body.String(), r1.Body.String())
	}

	// Same key + different body ⇒ 409 IDEMPOTENCY_KEY_REUSED.
	r3 := do("k1", `{"a":2}`)
	if r3.Code != 409 {
		t.Fatalf("reuse: status=%d want 409", r3.Code)
	}
	var e shoperr.Envelope
	_ = json.Unmarshal(r3.Body.Bytes(), &e)
	if e.Error.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("reuse code=%s", e.Error.Code)
	}

	if effects != 1 {
		t.Fatalf("effect ran %d times, want exactly 1", effects)
	}
}

func TestRequestHashStable(t *testing.T) {
	a := RequestHash("POST", "/kv", []byte(`{"x":1}`))
	b := RequestHash("POST", "/kv", []byte(`{"x":1}`))
	c := RequestHash("POST", "/kv", []byte(`{"x":2}`))
	if a != b {
		t.Fatal("same inputs must hash equal")
	}
	if a == c {
		t.Fatal("different bodies must hash differently")
	}
}
