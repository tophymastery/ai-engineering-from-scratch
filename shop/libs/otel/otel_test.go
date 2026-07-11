package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceparentRoundTrip(t *testing.T) {
	in := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := ParseTraceparent(in)
	if !ok {
		t.Fatal("valid traceparent rejected")
	}
	if sc.TraceIDHex() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id=%s", sc.TraceIDHex())
	}
	if sc.SpanIDHex() != "00f067aa0ba902b7" {
		t.Fatalf("span id=%s", sc.SpanIDHex())
	}
	if !sc.Sampled {
		t.Fatal("sampled flag lost")
	}
	if got := FormatTraceparent(sc); got != in {
		t.Fatalf("reformat=%s want %s", got, in)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"garbage",
		"00-tooshort-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", // 3 parts
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // reserved version
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",  // zero span id
		"00-zzzz2f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",  // non-hex
	}
	for _, v := range bad {
		if _, ok := ParseTraceparent(v); ok {
			t.Errorf("malformed %q accepted", v)
		}
	}
}

func TestStartSpanContinuesTrace(t *testing.T) {
	parent, _ := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := ContextWithSpan(context.Background(), parent)
	ctx, child := StartSpan(ctx, "op")
	if child.TraceID != parent.TraceID {
		t.Fatal("child must inherit trace id")
	}
	if child.SpanID == parent.SpanID {
		t.Fatal("child must have a fresh span id")
	}
	if TraceIDFromContext(ctx) != parent.TraceIDHex() {
		t.Fatal("accessor must return live trace id")
	}
}

func TestStartSpanNewRoot(t *testing.T) {
	ctx, sc := StartSpan(context.Background(), "root")
	if !sc.IsValid() {
		t.Fatal("root span must be valid")
	}
	if TraceIDFromContext(ctx) == "" || SpanIDFromContext(ctx) == "" {
		t.Fatal("root ids missing from context")
	}
}

func TestMiddlewarePropagates(t *testing.T) {
	var seenTrace string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTrace = TraceIDFromContext(r.Context())
		w.WriteHeader(200)
	}))
	// Incoming trace is continued.
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(TraceparentHeader, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seenTrace != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("ingress did not continue trace: %s", seenTrace)
	}
	if _, ok := ParseTraceparent(rec.Header().Get(TraceparentHeader)); !ok {
		t.Fatal("response must echo a valid traceparent")
	}
	// No incoming header ⇒ a fresh root trace is minted.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/y", nil))
	if seenTrace == "" {
		t.Fatal("missing header should start a root trace")
	}
}

func TestInjectExtract(t *testing.T) {
	_, sc := StartSpan(context.Background(), "op")
	h := http.Header{}
	Inject(h, sc)
	req := &http.Request{Header: h}
	got, ok := Extract(req)
	if !ok || got.TraceID != sc.TraceID || got.SpanID != sc.SpanID {
		t.Fatalf("inject/extract mismatch: %+v vs %+v", got, sc)
	}
}
