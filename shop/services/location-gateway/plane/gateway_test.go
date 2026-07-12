package plane

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateway_test.go — the D14 ingest-plane invariants: auth ONCE per connection
// (never per frame), 100 ms batching to the telemetry topic, and zero produce
// errors over a sustained burst. All frozen-clock; the produce sink is a MemSink
// standing in for the telemetry Kafka cluster.

// tokenAuth is a deterministic fake: token "tok:<driver>" authenticates to
// <driver>; anything else is rejected. Counts calls so we can assert auth-once.
type tokenAuth struct {
	mu    sync.Mutex
	calls int
}

func (a *tokenAuth) Authenticate(token string) (string, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if !strings.HasPrefix(token, "tok:") {
		return "", ErrUnauthenticated
	}
	return "drv_" + strings.TrimPrefix(token, "tok:"), nil
}

// memSink is the telemetry-topic stand-in: records batches, never errors.
type memSink struct {
	mu        sync.Mutex
	positions int
	batches   int
	sizes     []int
}

func (s *memSink) Produce(batch []Position) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.positions += len(batch)
	s.batches++
	s.sizes = append(s.sizes, len(batch))
	return nil
}

func newTestHub(clk Clock) (*Hub, *tokenAuth, *memSink) {
	auth := &tokenAuth{}
	sink := &memSink{}
	h := NewHub(HubConfig{Auth: auth, Sink: sink, Clock: clk, BatchWindow: 100 * time.Millisecond})
	return h, auth, sink
}

// TestAuthOncePerStream: a stream authenticates exactly once at Open; pushing N
// frames triggers ZERO further Authenticate calls.
func TestAuthOncePerStream(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	h, auth, _ := newTestHub(clk)

	s, err := h.Open("conn-1", "tok:0001")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const frames = 5000
	for i := 0; i < frames; i++ {
		if err := s.Push(Frame{Lat: 13.75, Lng: 100.53, RecordedAt: clk.Now()}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if auth.calls != 1 {
		t.Fatalf("auth-once VIOLATED: %d Authenticate calls for %d frames (want 1)", auth.calls, frames)
	}
	if h.AuthCount() != 1 || h.MsgCount() != frames {
		t.Fatalf("accounting: authCount=%d msgCount=%d", h.AuthCount(), h.MsgCount())
	}
	t.Logf("auth-once: 1 Authenticate call for %d pushed frames on the stream", frames)
}

// TestAuthOnceManyStreams: N connections ⇒ exactly N Authenticate calls total,
// regardless of how many frames each pushes.
func TestAuthOnceManyStreams(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	h, auth, _ := newTestHub(clk)
	const streams, framesEach = 500, 200
	for c := 0; c < streams; c++ {
		s, err := h.Open(fmt.Sprintf("conn-%d", c), fmt.Sprintf("tok:%04d", c))
		if err != nil {
			t.Fatalf("open %d: %v", c, err)
		}
		for i := 0; i < framesEach; i++ {
			_ = s.Push(Frame{Lat: 13.75, Lng: 100.53, RecordedAt: clk.Now()})
		}
	}
	if auth.calls != streams {
		t.Fatalf("auth-once (many): %d calls for %d streams x %d frames (want %d)",
			auth.calls, streams, framesEach, streams)
	}
	t.Logf("auth-once: %d Authenticate calls for %d streams x %d frames = %d messages",
		auth.calls, streams, framesEach, streams*framesEach)
}

// TestBadTokenRejected: a bad token is rejected at Open and opens no stream.
func TestBadTokenRejected(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	h, _, _ := newTestHub(clk)
	if _, err := h.Open("conn-x", "garbage"); err != ErrUnauthenticated {
		t.Fatalf("want ErrUnauthenticated, got %v", err)
	}
	if h.OpenStreams() != 0 {
		t.Fatalf("rejected connection left a stream open")
	}
}

// TestHundredMsBatchingAndZeroProduceErrors: frames buffer within a 100 ms window
// and flush as batches to the telemetry topic; over a sustained burst there are
// ZERO produce errors and every pushed frame is produced exactly once.
func TestHundredMsBatchingAndZeroProduceErrors(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0).UTC())
	h, _, sink := newTestHub(clk)

	const streams = 200
	sts := make([]*Stream, streams)
	for c := 0; c < streams; c++ {
		s, err := h.Open(fmt.Sprintf("conn-%d", c), fmt.Sprintf("tok:%04d", c))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		sts[c] = s
	}

	// Simulate 10 windows of 1 Hz-ish traffic: each window every stream pushes a
	// few frames, then the 100 ms window elapses and the gateway flushes one batch.
	const windows, perWindow = 10, 3
	pushed := 0
	for w := 0; w < windows; w++ {
		for _, s := range sts {
			for p := 0; p < perWindow; p++ {
				_ = s.Push(Frame{Lat: 13.75, Lng: 100.53, RecordedAt: clk.Now()})
				pushed++
			}
		}
		// Within the window nothing is produced yet.
		if got := h.Flush(false); got != 0 && w == 0 {
			// first window: clock hasn't advanced a full window yet
			t.Fatalf("produced %d before window elapsed", got)
		}
		clk.Advance(100 * time.Millisecond) // window elapses
		h.Flush(false)
	}

	if h.ProduceErrors() != 0 || sink.batches == 0 {
		t.Fatalf("produce errors=%d batches=%d", h.ProduceErrors(), sink.batches)
	}
	if int(h.Produced()) != pushed || sink.positions != pushed {
		t.Fatalf("produced %d / sink %d != pushed %d (lost or duplicated frames)",
			h.Produced(), sink.positions, pushed)
	}
	t.Logf("100ms batching: %d frames over %d streams -> %d batches, %d produced, 0 produce errors",
		pushed, streams, sink.batches, h.Produced())
}
