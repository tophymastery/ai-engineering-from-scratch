// Package logging emits the one 04 §3 network-log envelope from shared
// middleware — nobody hand-rolls request logs. It logs at the ingress (server
// received a request) and egress (this service called out) of every hop, reads
// the live trace/span from libs/otel, and applies per-route sampling classes
// (D27 / 04 §3): read paths are sampled, mutations and errors are ALWAYS logged.
//
// Structured JSON to stdout only (04 §3): the cluster pipeline ships it;
// services never manage log files. The envelope is versioned in
// contracts/log-schema.json and validated by logging_test.go — the standard is
// enforced, not aspirational.
package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/otel"
)

// Direction is the 04 §3 direction enum.
type Direction string

const (
	Ingress Direction = "ingress"
	Egress  Direction = "egress"
	Consume Direction = "consume"
	Produce Direction = "produce"
)

// Protocol is the 04 §3 protocol enum.
type Protocol string

const (
	HTTP  Protocol = "http"
	GRPC  Protocol = "grpc"
	Kafka Protocol = "kafka"
)

// Actor is the 04 §3 actor object (prefixed IDs only — never PII).
type Actor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ErrorInfo is the 04 §3 error object.
type ErrorInfo struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

// Entry is the 04 §3 log envelope. Field order/names/JSON tags are the versioned
// contract (contracts/log-schema.json).
type Entry struct {
	TS        string     `json:"ts"`
	Level     string     `json:"level"`
	Service   string     `json:"service"`
	Version   string     `json:"version"`
	Env       string     `json:"env"`
	Region    string     `json:"region"`
	Direction Direction  `json:"direction"`
	Protocol  Protocol   `json:"protocol"`
	Route     string     `json:"route"`
	Peer      string     `json:"peer"`
	Status    int        `json:"status"`
	LatencyMS int64      `json:"latency_ms"`
	BytesIn   int64      `json:"bytes_in"`
	BytesOut  int64      `json:"bytes_out"`
	TraceID   string     `json:"trace_id"`
	SpanID    string     `json:"span_id"`
	RequestID string     `json:"request_id"`
	Actor     *Actor     `json:"actor"`
	Keys      map[string]string `json:"keys"`
	Error     *ErrorInfo `json:"error"`
}

// Config identifies the emitting service (04 §3 static fields) and where lines
// go. SampleRate is the fraction of read-path INFO kept (D27: 1–5% in prod);
// mutations, errors and WARN+ ignore it.
type Config struct {
	Service    string
	Version    string
	Env        string
	Region     string
	Out        io.Writer // defaults to os.Stdout
	SampleRate float64   // 0..1 for read-path INFO; default 1.0 (keep all)
	// Sampler decides per (method, route, status) whether to keep the line.
	// Defaults to DefaultSampler(SampleRate).
	Sampler Sampler
}

// Logger emits envelopes for a single service.
type Logger struct {
	cfg  Config
	out  io.Writer
	mu   sync.Mutex
	enc  *json.Encoder
	Now  func() time.Time // overridable for tests
	Rand func() float64
}

// New builds a Logger from cfg, filling defaults.
func New(cfg Config) *Logger {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 1.0
	}
	if cfg.Sampler == nil {
		cfg.Sampler = DefaultSampler(cfg.SampleRate)
	}
	l := &Logger{cfg: cfg, out: cfg.Out, enc: json.NewEncoder(cfg.Out), Now: time.Now}
	l.Rand = defaultRand
	return l
}

// Emit writes one entry, filling the static service fields and timestamp, after
// the sampler's keep decision. It is safe for concurrent use.
func (l *Logger) Emit(e Entry) {
	e.Service = l.cfg.Service
	e.Version = l.cfg.Version
	e.Env = l.cfg.Env
	e.Region = l.cfg.Region
	if e.TS == "" {
		e.TS = l.Now().UTC().Format(time.RFC3339Nano)
	}
	if e.Level == "" {
		e.Level = levelFor(e.Status)
	}
	if e.Keys == nil {
		e.Keys = map[string]string{}
	}
	if !l.cfg.Sampler.Keep(e, l.Rand()) {
		return
	}
	l.mu.Lock()
	_ = l.enc.Encode(e)
	l.mu.Unlock()
}

func levelFor(status int) string {
	switch {
	case status == 0:
		return "INFO"
	case status >= 500:
		return "ERROR"
	case status >= 400:
		return "WARN"
	default:
		return "INFO"
	}
}

// traceIDFor exposes the otel accessor so libs/errors can embed the same
// trace_id in error envelopes.
func TraceIDFromRequest(r *http.Request) string {
	return otel.TraceIDFromContext(r.Context())
}
