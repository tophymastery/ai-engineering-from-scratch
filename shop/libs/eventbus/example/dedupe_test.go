package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// TestDuplicateDeliveryBurst redelivers every event 10 extra times straight onto
// the bus (worst-case at-least-once). The SQL inbox must yield ZERO duplicate
// side effects: the projection holds exactly one row per order.
func TestDuplicateDeliveryBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := NewSQLPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := p.Start(ctx, p.DefaultConsumer())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	const n = 300
	for i := 0; i < n; i++ {
		if _, err := p.PublishOrder(ctx, 100+i); err != nil {
			t.Fatal(err)
		}
	}
	// Let the normal path deliver once.
	waitConsumed(t, p, n, 10*time.Second)

	// Snapshot every emitted message from the outbox and REDELIVER each 10x,
	// concurrently, jumping straight onto the bus (bypassing the cursor) — the
	// redelivery a flaky broker / relay restart would produce.
	recs, err := p.Store().Tail(ctx, 0, n+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("expected %d outbox rows, got %d", n, len(recs))
	}
	var wg sync.WaitGroup
	for _, r := range recs {
		m, err := r.Message()
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 10; k++ {
			wg.Add(1)
			go func(m eventbus.Message) {
				defer wg.Done()
				if err := p.bus.Publish(ctx, m); err != nil {
					t.Errorf("republish: %v", err)
				}
			}(m)
		}
	}
	wg.Wait()

	// Give redeliveries time to be processed-and-deduped.
	time.Sleep(300 * time.Millisecond)
	deadline := time.After(10 * time.Second)
	for p.bus.PublishedCount() < int64(n*11) { // 1 normal + 10 redeliveries
		select {
		case <-deadline:
			t.Fatalf("republish not fully accepted: %d", p.bus.PublishedCount())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Settle: consumer drains the redelivery backlog.
	time.Sleep(500 * time.Millisecond)

	orders, view, _, consumed := p.Audit(ctx)
	if orders != n || view != n {
		t.Fatalf("dedupe failed: orders=%d projection=%d want %d each (duplicate side effect!)", orders, view, n)
	}
	if consumed != int64(n) {
		t.Fatalf("consumed(applied)=%d want %d — inbox let a duplicate through", consumed, n)
	}
	// Prove the burst was real: total deliveries were 11x the effects.
	if p.bus.PublishedCount() != int64(n*11) {
		t.Fatalf("expected %d deliveries, saw %d", n*11, p.bus.PublishedCount())
	}
	t.Logf("[dedupe] %d events x11 deliveries => %d unique effects (projection rows=%d), zero duplicates", n, consumed, view)
}

func waitConsumed(t *testing.T, p *SQLPipeline, n int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if _, _, _, consumed := p.Audit(context.Background()); consumed >= int64(n) {
			return
		}
		select {
		case <-deadline:
			_, _, _, consumed := p.Audit(context.Background())
			t.Fatalf("only %d/%d consumed before timeout", consumed, n)
		case <-time.After(3 * time.Millisecond):
		}
	}
}
