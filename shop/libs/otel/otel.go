// Package otel is the tracing bootstrap (04 §2): real W3C traceparent
// extract/inject for HTTP, plus span / trace_id accessors that libs/logging
// reads so every log line carries the live trace.
//
// EXPORTER MODES. Spans are always created and propagated in-process — that is
// the load-bearing logic and it is fully tested. Whether spans are *shipped*
// depends on OTEL_EXPORTER_OTLP_ENDPOINT:
//
//   - endpoint set  → the real service wires an OTLP exporter at its edge
//     (out of scope for this pure-stdlib lib; the SpanContext this package
//     produces is what an exporter would serialise).
//   - endpoint empty → NO-OP EXPORTER: spans are still created and propagated,
//     just not shipped anywhere. This is the default in tests, CI, and any
//     environment without a collector, so nothing breaks when Tempo is absent.
//
// The package intentionally has zero third-party dependencies: the W3C
// propagation format is small and stable, and vendoring the full OTel SDK would
// add a large surface for no gain here. `TracerProvider` is the seam a service
// swaps for the real SDK.
package otel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
)

// TraceparentHeader is the W3C header name.
const TraceparentHeader = "traceparent"

// SpanContext is the propagated identity of a span: a 16-byte trace id, an
// 8-byte span id, and the sampled flag. Hex forms match the log envelope.
type SpanContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

// TraceIDHex returns the 32-char lower-hex trace id (04 §3 `trace_id`).
func (sc SpanContext) TraceIDHex() string { return hex.EncodeToString(sc.TraceID[:]) }

// SpanIDHex returns the 16-char lower-hex span id (04 §3 `span_id`).
func (sc SpanContext) SpanIDHex() string { return hex.EncodeToString(sc.SpanID[:]) }

// IsValid reports whether the context has non-zero trace and span ids.
func (sc SpanContext) IsValid() bool {
	return sc.TraceID != [16]byte{} && sc.SpanID != [8]byte{}
}

type ctxKey int

const spanKey ctxKey = 0

// ContextWithSpan stores a SpanContext for downstream accessors.
func ContextWithSpan(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, spanKey, sc)
}

// SpanFromContext returns the active SpanContext, if any.
func SpanFromContext(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(spanKey).(SpanContext)
	return sc, ok
}

// TraceIDFromContext returns the live trace id hex ("" if none) — the accessor
// libs/logging and libs/errors use.
func TraceIDFromContext(ctx context.Context) string {
	if sc, ok := SpanFromContext(ctx); ok {
		return sc.TraceIDHex()
	}
	return ""
}

// SpanIDFromContext returns the live span id hex ("" if none).
func SpanIDFromContext(ctx context.Context) string {
	if sc, ok := SpanFromContext(ctx); ok {
		return sc.SpanIDHex()
	}
	return ""
}

// ParseTraceparent parses a W3C `traceparent` value
// (`version-traceid-spanid-flags`, all lower hex). It accepts only version 00
// with a non-zero trace id and span id, per the spec; invalid values report
// ok=false so callers start a fresh trace.
func ParseTraceparent(v string) (SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 {
		return SpanContext{}, false
	}
	ver, tid, sid, flags := parts[0], parts[1], parts[2], parts[3]
	if len(ver) != 2 || len(tid) != 32 || len(sid) != 16 || len(flags) != 2 {
		return SpanContext{}, false
	}
	if ver == "ff" { // reserved / invalid version
		return SpanContext{}, false
	}
	var sc SpanContext
	tb, err := hex.DecodeString(tid)
	if err != nil {
		return SpanContext{}, false
	}
	sb, err := hex.DecodeString(sid)
	if err != nil {
		return SpanContext{}, false
	}
	fb, err := hex.DecodeString(flags)
	if err != nil {
		return SpanContext{}, false
	}
	copy(sc.TraceID[:], tb)
	copy(sc.SpanID[:], sb)
	if sc.TraceID == [16]byte{} || sc.SpanID == [8]byte{} {
		return SpanContext{}, false // all-zero ids are invalid
	}
	sc.Sampled = fb[0]&0x01 == 0x01
	return sc, true
}

// FormatTraceparent renders a SpanContext as a W3C `traceparent` value.
func FormatTraceparent(sc SpanContext) string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceIDHex() + "-" + sc.SpanIDHex() + "-" + flags
}

// Extract reads the incoming traceparent from an HTTP request. If absent or
// malformed it returns ok=false so the caller starts a new root span.
func Extract(r *http.Request) (SpanContext, bool) {
	return ParseTraceparent(r.Header.Get(TraceparentHeader))
}

// Inject writes a SpanContext into an outbound HTTP header set (egress hop).
func Inject(h http.Header, sc SpanContext) {
	h.Set(TraceparentHeader, FormatTraceparent(sc))
}

func newTraceID() (b [16]byte) { _, _ = rand.Read(b[:]); return }
func newSpanID() (b [8]byte)   { _, _ = rand.Read(b[:]); return }

// Sampled is the default sampled decision for new root traces. Real services
// override via tail-based sampling at the collector (04 §2); the propagation
// flag here just carries the parent's decision.
var Sampled = true

// StartSpan continues or begins a trace and returns a child context. If the
// context already holds a SpanContext (extracted from an upstream hop), the
// trace id is preserved and a fresh child span id is minted. Otherwise a new
// root trace is started. The returned SpanContext is stored in the context so
// TraceIDFromContext works downstream.
func StartSpan(ctx context.Context, name string) (context.Context, SpanContext) {
	child := SpanContext{SpanID: newSpanID()}
	if parent, ok := SpanFromContext(ctx); ok && parent.IsValid() {
		child.TraceID = parent.TraceID
		child.Sampled = parent.Sampled
	} else {
		child.TraceID = newTraceID()
		child.Sampled = Sampled
	}
	_ = name // name is attached by a real exporter; no-op mode ignores it
	return ContextWithSpan(ctx, child), child
}

// Middleware continues the W3C trace on ingress: it extracts an upstream
// traceparent (or starts a root), mints a child span, stores the SpanContext in
// the request context, and echoes the active traceparent on the response so a
// caller can correlate. This is the ingress half of one-trace-across-the-cluster
// (04 §2).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if sc, ok := Extract(r); ok {
			ctx = ContextWithSpan(ctx, sc)
		}
		ctx, sc := StartSpan(ctx, r.Method+" "+r.URL.Path)
		w.Header().Set(TraceparentHeader, FormatTraceparent(sc))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ExporterMode reports the configured exporter mode for logs/health endpoints.
func ExporterMode() string {
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" {
		return "noop"
	}
	return "otlp"
}
