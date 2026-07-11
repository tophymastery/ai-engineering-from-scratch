package eventbus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkEnv(t *testing.T, id, aggID string) Envelope {
	t.Helper()
	env, err := NewEnvelope(id, "order.paid", "trace-"+id, Aggregate{Type: "order", ID: aggID, Region: "bkk"}, 1, map[string]any{"n": 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func mkMsg(t *testing.T, id, aggID string) Message {
	t.Helper()
	m, err := NewMessage("order.paid", mkEnv(t, id, aggID))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestOrderedPerKey: all events for one aggregate arrive in publish order.
func TestOrderedPerKey(t *testing.T) {
	b := NewMemBroker(WithPartitions(8))
	var mu sync.Mutex
	got := map[string][]string{}
	done := make(chan struct{})
	var seen atomic.Int64
	total := 300

	sub, err := b.Subscribe(SubscribeConfig{Topic: "order.paid", Group: "g1"}, func(_ context.Context, m Message) error {
		mu.Lock()
		got[m.Key] = append(got[m.Key], m.Envelope.EventID)
		mu.Unlock()
		if seen.Add(1) == int64(total) {
			close(done)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// 3 aggregates, 100 events each, interleaved.
	for i := 0; i < 100; i++ {
		for _, agg := range []string{"ord_A", "ord_B", "ord_C"} {
			if err := b.Publish(context.Background(), mkMsg(t, fmt.Sprintf("%s-%03d", agg, i), agg)); err != nil {
				t.Fatal(err)
			}
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: saw %d/%d", seen.Load(), total)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, agg := range []string{"ord_A", "ord_B", "ord_C"} {
		ids := got[agg]
		if len(ids) != 100 {
			t.Fatalf("%s: got %d events", agg, len(ids))
		}
		for i, id := range ids {
			want := fmt.Sprintf("%s-%03d", agg, i)
			if id != want {
				t.Fatalf("%s out of order at %d: got %s want %s", agg, i, id, want)
			}
		}
	}
}

// TestAtLeastOnceRetry: a handler that fails twice then succeeds still acks; no
// park.
func TestAtLeastOnceRetry(t *testing.T) {
	b := NewMemBroker(WithPartitions(2))
	dlq := NewMemDLQ()
	var attempts atomic.Int64
	done := make(chan struct{})
	sub, _ := b.Subscribe(SubscribeConfig{Topic: "order.paid", Group: "g", DLQ: dlq}, func(_ context.Context, m Message) error {
		n := attempts.Add(1)
		if n < 3 {
			return fmt.Errorf("transient %d", n)
		}
		close(done)
		return nil
	})
	defer sub.Close()
	_ = b.Publish(context.Background(), mkMsg(t, "e1", "ord_X"))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never succeeded")
	}
	if d := dlq.Depth(); d != 0 {
		t.Fatalf("expected no park, DLQ depth=%d", d)
	}
}

// TestPoisonParksWithoutBlockingPartition: one permanently-failing event parks
// after MaxAttempts while later events on the SAME partition keep flowing.
func TestPoisonParksWithoutBlockingPartition(t *testing.T) {
	// One partition forces all keys onto the same worker — the strict
	// head-of-line-block test.
	b := NewMemBroker(WithPartitions(1))
	dlq := NewMemDLQ()
	var okCount atomic.Int64
	done := make(chan struct{})
	sub, _ := b.Subscribe(SubscribeConfig{Topic: "order.paid", Group: "g", MaxAttempts: 3, DLQ: dlq}, func(_ context.Context, m Message) error {
		if m.Envelope.EventID == "poison" {
			return fmt.Errorf("permanent failure")
		}
		if okCount.Add(1) == 5 {
			close(done)
		}
		return nil
	})
	defer sub.Close()

	_ = b.Publish(context.Background(), mkMsg(t, "poison", "ord_1"))
	for i := 0; i < 5; i++ {
		_ = b.Publish(context.Background(), mkMsg(t, fmt.Sprintf("ok-%d", i), "ord_1"))
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("partition blocked: only %d/5 good events flowed", okCount.Load())
	}
	if d := dlq.Depth(); d != 1 {
		t.Fatalf("expected 1 parked, got %d", d)
	}
	if p := dlq.List()[0]; p.Attempts != 3 || p.Message.Envelope.EventID != "poison" {
		t.Fatalf("unexpected parked record: %+v", p)
	}
}

// TestTwoGroupsIndependent: two consumer groups each see every message.
func TestTwoGroupsIndependent(t *testing.T) {
	b := NewMemBroker(WithPartitions(4))
	var a, c atomic.Int64
	sub1, _ := b.Subscribe(SubscribeConfig{Topic: "order.paid", Group: "A"}, func(_ context.Context, _ Message) error { a.Add(1); return nil })
	sub2, _ := b.Subscribe(SubscribeConfig{Topic: "order.paid", Group: "C"}, func(_ context.Context, _ Message) error { c.Add(1); return nil })
	defer sub1.Close()
	defer sub2.Close()
	for i := 0; i < 50; i++ {
		_ = b.Publish(context.Background(), mkMsg(t, fmt.Sprintf("e%d", i), fmt.Sprintf("ord_%d", i%7)))
	}
	deadline := time.After(3 * time.Second)
	for a.Load() < 50 || c.Load() < 50 {
		select {
		case <-deadline:
			t.Fatalf("groups did not both reach 50: A=%d C=%d", a.Load(), c.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}
}
