package logging

import (
	"net/http"

	"github.com/shop-platform/shop/libs/otel"
)

// RequestIDHeader is minted once at the gateway and propagated on every hop
// (04 §3). Middleware echoes it back to the client in the same header.
const RequestIDHeader = "X-Request-Id"

// EnrichFunc lets a service attach the actor and business keys (prefixed IDs
// only — never PII) to the ingress entry after the handler runs.
type EnrichFunc func(r *http.Request, e *Entry)

// statusRecorder captures status + response bytes for the egress summary.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Middleware logs the 04 §3 ingress envelope for every request this service
// receives: it records latency/status/bytes, pulls trace_id/span_id from the
// otel SpanContext (wrap with otel.Middleware first), and honours the
// request_id header (echoing it to the client). Sampling classes apply — a
// sampled-out healthy read emits nothing; a mutation or error always logs.
//
// enrich (optional) attaches actor + business keys once known.
func (l *Logger) Middleware(enrich EnrichFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := l.Now()
			reqID := r.Header.Get(RequestIDHeader)
			if reqID != "" {
				w.Header().Set(RequestIDHeader, reqID)
			}
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			e := Entry{
				Direction: Ingress,
				Protocol:  HTTP,
				Route:     r.Method + " " + routeOf(r),
				Peer:      r.Header.Get("X-Peer"),
				Status:    rec.status,
				LatencyMS: l.Now().Sub(start).Milliseconds(),
				BytesIn:   r.ContentLength,
				BytesOut:  rec.bytes,
				TraceID:   otel.TraceIDFromContext(r.Context()),
				SpanID:    otel.SpanIDFromContext(r.Context()),
				RequestID: reqID,
				Keys:      map[string]string{},
			}
			if enrich != nil {
				enrich(r, &e)
			}
			l.Emit(e)
		})
	}
}

// routeOf returns a low-cardinality route label. Services with path params
// should set a templated pattern; here we default to the raw path.
func routeOf(r *http.Request) string {
	if p := r.Header.Get("X-Route-Pattern"); p != "" {
		return p
	}
	return r.URL.Path
}

// roundTripper logs the 04 §3 EGRESS envelope for every outbound HTTP call this
// service makes, and injects the current trace so the downstream hop continues
// it (one-trace-across-the-cluster, 04 §2).
type roundTripper struct {
	l    *Logger
	peer string
	next http.RoundTripper
}

// WrapClient wraps an http.Client's transport so its outbound calls emit egress
// logs and carry the trace. peer names the callee.
func (l *Logger) WrapClient(c *http.Client, peer string) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp := *c
	cp.Transport = &roundTripper{l: l, peer: peer, next: base}
	return &cp
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := rt.l.Now()
	if sc, ok := otel.SpanFromContext(req.Context()); ok {
		otel.Inject(req.Header, sc)
	}
	resp, err := rt.next.RoundTrip(req)
	e := Entry{
		Direction: Egress,
		Protocol:  HTTP,
		Route:     req.Method + " " + req.URL.Path,
		Peer:      rt.peer,
		LatencyMS: rt.l.Now().Sub(start).Milliseconds(),
		BytesIn:   0,
		BytesOut:  req.ContentLength,
		TraceID:   otel.TraceIDFromContext(req.Context()),
		SpanID:    otel.SpanIDFromContext(req.Context()),
		RequestID: req.Header.Get(RequestIDHeader),
	}
	if err != nil {
		e.Status = 0
		e.Level = "ERROR"
		e.Error = &ErrorInfo{Code: "UPSTREAM_UNREACHABLE", Retryable: true}
	} else {
		e.Status = resp.StatusCode
		e.BytesIn = resp.ContentLength
	}
	rt.l.Emit(e)
	return resp, err
}
