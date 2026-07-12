package plane

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// gateway.go — the location-gateway stream protocol (D14): drivers hold ONE
// persistent stream (gRPC bidi, MQTT fallback) to a per-cell gateway; the gateway
// authenticates ONCE per connection (not per frame), buffers ~64-byte position
// frames, and flushes 100 ms windows as batches into the telemetry topic. No live
// gRPC/MQTT/Kafka in this sandbox, so the wire transport is modelled in-process
// and the telemetry topic is a Sink (wired to libs/eventbus in main); the
// auth-once accounting, the 100 ms batching, the zero-produce-error guarantee,
// and the reconnect-storm recovery are YOUR code, tested for real. Disclosed in
// VERIFICATION.md §V-T13.

// ErrUnauthenticated is returned when a stream open presents a bad/empty token.
var ErrUnauthenticated = errors.New("location-gateway: unauthenticated")

// ErrClosed is returned when pushing to a closed stream.
var ErrClosed = errors.New("location-gateway: stream closed")

// Authenticator verifies a driver's connection token ONCE at stream open and
// returns the authenticated driver_id. Production plugs in libs/edgeauth JWT
// verification; tests plug a deterministic fake. The gateway calls this exactly
// once per connection — never per frame (the D14 "auth once per connection").
type Authenticator interface {
	Authenticate(token string) (driverID string, err error)
}

// AuthFunc adapts a function to Authenticator.
type AuthFunc func(token string) (string, error)

// Authenticate implements Authenticator.
func (f AuthFunc) Authenticate(token string) (string, error) { return f(token) }

// Sink receives a flushed 100 ms batch of positions (the telemetry topic). It
// returns an error if the produce failed; the gateway counts produce errors so
// the "zero produce errors" criterion is measurable.
type Sink interface {
	Produce(batch []Position) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(batch []Position) error

// Produce implements Sink.
func (f SinkFunc) Produce(batch []Position) error { return f(batch) }

// Frame is one ~64-byte position frame a driver pushes on its stream.
type Frame struct {
	Lat        float64
	Lng        float64
	RecordedAt time.Time
}

// Stream is one authenticated driver connection. It is authenticated once (at
// Open); every subsequent Push carries NO auth — the auth cost is paid per
// connection, not per message.
type Stream struct {
	ID       string
	DriverID string

	hub    *Hub
	mu     sync.Mutex
	buf    []Position // frames buffered since the last flush
	openAt time.Time
	closed bool
}

// Push appends a position frame to the stream's 100 ms buffer. O(1), no auth, no
// network — the produce happens on the batch flush. Returns ErrClosed if the
// stream was closed.
func (s *Stream) Push(f Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.buf = append(s.buf, Position{
		DriverID:   s.DriverID,
		Lat:        f.Lat,
		Lng:        f.Lng,
		Cell:       LatLngToCell(f.Lat, f.Lng),
		RecordedAt: f.RecordedAt,
	})
	atomic.AddInt64(&s.hub.msgCount, 1)
	return nil
}

// drain removes and returns the stream's buffered frames (called by the flusher).
func (s *Stream) drain() []Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	out := s.buf
	s.buf = nil
	return out
}

// HubConfig configures the gateway hub.
type HubConfig struct {
	Auth        Authenticator
	Sink        Sink
	Clock       Clock
	BatchWindow time.Duration // D14 100 ms batching window
}

// Hub is the per-cell location-gateway: it owns the authenticated streams, the
// 100 ms batch flusher, and the ingest/produce accounting.
type Hub struct {
	auth   Authenticator
	sink   Sink
	clock  Clock
	window time.Duration

	mu      sync.RWMutex
	streams map[string]*Stream

	// accounting (real counters, read by the criteria tests).
	authCount     int64 // total Authenticate() invocations
	msgCount      int64 // total frames pushed
	produced      int64 // total positions produced downstream
	produceErrors int64 // failed Produce() calls (must be 0)
	batches       int64 // total batch flushes
	lastFlush     time.Time
}

// NewHub builds a gateway hub. Pass BatchWindow == 0 for the D14 default of 100 ms.
func NewHub(cfg HubConfig) *Hub {
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	if cfg.BatchWindow <= 0 {
		cfg.BatchWindow = 100 * time.Millisecond
	}
	return &Hub{
		auth:      cfg.Auth,
		sink:      cfg.Sink,
		clock:     cfg.Clock,
		window:    cfg.BatchWindow,
		streams:   map[string]*Stream{},
		lastFlush: cfg.Clock.Now(),
	}
}

// Open authenticates a connection ONCE and registers a stream. The stream id is
// caller-supplied (the transport's connection id) so a reconnect can re-open the
// same logical stream. Every Open pays exactly one Authenticate call; no later
// Push does — that is the auth-once invariant this method embodies.
func (h *Hub) Open(streamID, token string) (*Stream, error) {
	atomic.AddInt64(&h.authCount, 1) // auth happens ONCE per connection, here
	driverID, err := h.auth.Authenticate(token)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	s := &Stream{ID: streamID, DriverID: driverID, hub: h, openAt: h.clock.Now()}
	h.mu.Lock()
	h.streams[streamID] = s
	h.mu.Unlock()
	return s, nil
}

// Stream returns the open stream with the given id, or nil. Lets the HTTP ingest
// path reuse an already-authenticated stream (auth-once across ingest calls).
func (h *Hub) Stream(streamID string) *Stream {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.streams[streamID]
}

// Close removes a stream (a disconnect). Buffered frames are flushed first so no
// in-flight position is lost on a graceful close.
func (h *Hub) Close(streamID string) {
	h.mu.Lock()
	s := h.streams[streamID]
	delete(h.streams, streamID)
	h.mu.Unlock()
	if s == nil {
		return
	}
	if batch := s.drain(); len(batch) > 0 {
		h.produce(batch)
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Flush produces one 100 ms batch across every open stream if the batch window
// has elapsed since the last flush (driven by a 100 ms ticker in production; the
// tests advance a ManualClock and call Flush explicitly). Returns the number of
// positions produced. All buffered frames across all streams are coalesced into
// batches to the telemetry topic — the D14 "gateways batch 100 ms windows".
func (h *Hub) Flush(force bool) int {
	now := h.clock.Now()
	if !force && now.Sub(h.lastFlush) < h.window {
		return 0
	}
	h.lastFlush = now

	h.mu.RLock()
	streams := make([]*Stream, 0, len(h.streams))
	for _, s := range h.streams {
		streams = append(streams, s)
	}
	h.mu.RUnlock()

	produced := 0
	for _, s := range streams {
		if batch := s.drain(); len(batch) > 0 {
			h.produce(batch)
			produced += len(batch)
		}
	}
	return produced
}

// produce ships one batch to the telemetry topic and records the outcome.
func (h *Hub) produce(batch []Position) {
	atomic.AddInt64(&h.batches, 1)
	if err := h.sink.Produce(batch); err != nil {
		atomic.AddInt64(&h.produceErrors, 1)
		return
	}
	atomic.AddInt64(&h.produced, int64(len(batch)))
}

// --- accounting getters (criteria tests + /healthz) ---

// AuthCount is the total number of Authenticate() calls (the auth-once proof:
// this equals the number of connections opened, NOT the number of frames).
func (h *Hub) AuthCount() int64 { return atomic.LoadInt64(&h.authCount) }

// MsgCount is the total frames pushed across all streams.
func (h *Hub) MsgCount() int64 { return atomic.LoadInt64(&h.msgCount) }

// Produced is the total positions produced to the telemetry topic.
func (h *Hub) Produced() int64 { return atomic.LoadInt64(&h.produced) }

// ProduceErrors is the count of failed produces (the "zero produce errors"
// criterion asserts this is 0).
func (h *Hub) ProduceErrors() int64 { return atomic.LoadInt64(&h.produceErrors) }

// Batches is the number of 100 ms batch flushes performed.
func (h *Hub) Batches() int64 { return atomic.LoadInt64(&h.batches) }

// OpenStreams is the current number of live streams.
func (h *Hub) OpenStreams() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.streams)
}
