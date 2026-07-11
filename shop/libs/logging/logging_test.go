package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/otel"
)

func testLogger(buf *bytes.Buffer, rate float64) *Logger {
	l := New(Config{
		Service: "placeholder", Version: "1.42.0", Env: "test", Region: "bkk",
		Out: buf, SampleRate: rate, Sampler: DefaultSampler(rate),
	})
	fixed := time.Date(2026, 7, 10, 2, 15, 0, 0, time.UTC)
	l.Now = func() time.Time { return fixed }
	return l
}

// Every emitted line must validate against contracts/log-schema.json.
func TestEmittedLinesMatchSchema(t *testing.T) {
	sch, err := loadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	var buf bytes.Buffer
	l := testLogger(&buf, 1.0)

	// Exercise ingress middleware across a mutation, a read, and an error, with
	// otel providing a live trace and an enrich func attaching actor + keys.
	h := otel.Middleware(l.Middleware(func(r *http.Request, e *Entry) {
		e.Actor = &Actor{Type: "customer", ID: "usr_01H"}
		e.Keys["order_id"] = "ord_01H"
		if r.URL.Path == "/boom" {
			e.Error = &ErrorInfo{Code: "INTERNAL", Retryable: false}
		}
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			w.WriteHeader(500)
		case "/orders":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"order_id":"ord_01H"}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	})))

	for _, tc := range []struct{ method, path string }{
		{"POST", "/orders"}, {"GET", "/home"}, {"GET", "/boom"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("x"))
		req.Header.Set(RequestIDHeader, "req_01H8")
		if tc.path == "/boom" {
			req = httptest.NewRequest("GET", "/boom", nil)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("want 3 log lines, got %d: %s", len(lines), buf.String())
	}
	for i, ln := range lines {
		if v := validateLine(sch, ln); len(v) > 0 {
			t.Errorf("line %d violates log-schema: %v\n  line=%s", i, v, ln)
		}
	}
}

// A malformed entry (bad enum) must be REJECTED by the validator — proves the
// schema test can actually fail, not just rubber-stamp.
func TestSchemaCatchesViolation(t *testing.T) {
	sch, _ := loadSchema()
	bad := `{"ts":"t","level":"TRACE","service":"s","version":"v","env":"test","region":"r","direction":"ingress","protocol":"http","route":"GET /x","peer":"","status":200,"latency_ms":1,"bytes_in":0,"bytes_out":0,"trace_id":"","span_id":"","request_id":"","actor":null,"keys":{},"error":null}`
	if v := validateLine(sch, []byte(bad)); len(v) == 0 {
		t.Fatal("validator accepted an invalid level (TRACE)")
	}
	missing := `{"level":"INFO"}`
	if v := validateLine(sch, []byte(missing)); len(v) == 0 {
		t.Fatal("validator accepted an entry missing required fields")
	}
}

// Read paths are sampled; mutations and errors are always logged (04 §3 / D27).
func TestSamplingClasses(t *testing.T) {
	var buf bytes.Buffer
	l := testLogger(&buf, 0.0) // drop ALL sampled read paths
	// Force the read-path random draw high so nothing is kept by chance.
	l.Rand = func() float64 { return 0.999 }

	emit := func(method, route string, status int) {
		l.Emit(Entry{Direction: Ingress, Protocol: HTTP, Route: method + " " + route, Status: status})
	}
	emit("GET", "/home", 200)       // read → dropped (rate 0)
	emit("POST", "/orders", 201)    // mutation → kept
	emit("GET", "/home", 500)       // error → kept
	emit("DELETE", "/orders/1", 204) // mutation → kept

	var kept []Entry
	for _, ln := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(ln) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(ln, &e); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, e)
	}
	if len(kept) != 3 {
		t.Fatalf("want 3 kept (2 mutations + 1 error), got %d: %s", len(kept), buf.String())
	}
	for _, e := range kept {
		if strings.HasPrefix(e.Route, "GET") && e.Status < 400 {
			t.Errorf("a healthy read path leaked past sampling: %+v", e)
		}
	}
}

// Read paths ARE kept when the draw is under the rate.
func TestSamplingKeepsReadUnderRate(t *testing.T) {
	var buf bytes.Buffer
	l := testLogger(&buf, 0.05)
	l.Rand = func() float64 { return 0.01 } // under 0.05 → keep
	l.Emit(Entry{Direction: Ingress, Protocol: HTTP, Route: "GET /home", Status: 200})
	if buf.Len() == 0 {
		t.Fatal("read path under sample rate should be kept")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		route  string
		status int
		want   Class
	}{
		{"POST /orders", 201, ClassMutation},
		{"PATCH /orders/1", 200, ClassMutation},
		{"DELETE /x", 204, ClassMutation},
		{"GET /home", 200, ClassRead},
		{"GET /home", 500, ClassError},
		{"POST /orders", 422, ClassError},
	}
	for _, c := range cases {
		got := Classify(Entry{Route: c.route, Status: c.status, Level: levelFor(c.status)})
		if got != c.want {
			t.Errorf("Classify(%q,%d)=%d want %d", c.route, c.status, got, c.want)
		}
	}
}

func TestTraceIDFromRequest(t *testing.T) {
	ctx := otel.ContextWithSpan(context.Background(), func() otel.SpanContext {
		sc, _ := otel.ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		return sc
	}())
	r := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	if got := TraceIDFromRequest(r); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id=%s", got)
	}
}
