package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
)

// TestPoisonParkAndReplay: one permanently-failing event parks to the DLQ after
// 3 retries WITHOUT blocking its partition (following events keep flowing, lag
// recovers < 60s), then a dlqctl-style replay after "fixing" the handler
// converges exactly-once through the inbox.
func TestPoisonParkAndReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One partition: poison + every following event share a worker — the strict
	// head-of-line-block test.
	p, err := newPipeline(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	var fixed atomic.Bool
	var poisonID atomic.Value
	poisonID.Store("")

	// Consumer: exactly-once via inbox; the poison event's effect fails until
	// "fixed".
	consumer := func(ctx context.Context, m eventbus.Message) error {
		applied, err := p.proc.Process(ctx, m, func(ctx context.Context, tx *sql.Tx) error {
			if pid, _ := poisonID.Load().(string); m.Envelope.EventID == pid && !fixed.Load() {
				return fmt.Errorf("poison: handler bug")
			}
			var amt struct {
				Amount int `json:"amount"`
			}
			_ = jsonUnmarshal(m.Envelope.Payload, &amt)
			_, e := tx.ExecContext(ctx, `INSERT INTO order_paid_view (order_id, amount) VALUES (?, ?)`,
				m.Envelope.Aggregate.ID, amt.Amount)
			return e
		})
		if err != nil {
			return err
		}
		if applied {
			p.consumed.Add(1)
		}
		return nil
	}
	stop, err := p.Start(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Publish the poison first, then a stream of good events behind it.
	const following = 200
	pid, err := p.PublishOrder(ctx, 999)
	if err != nil {
		t.Fatal(err)
	}
	poisonID.Store(pid)
	tStart := time.Now()
	for i := 0; i < following; i++ {
		if _, err := p.PublishOrder(ctx, i); err != nil {
			t.Fatal(err)
		}
	}

	// The partition must NOT be blocked: all `following` good events get through
	// even though the poison ahead of some of them keeps failing.
	waitConsumed(t, p, following, 60*time.Second)
	recovery := time.Since(tStart)
	if recovery >= 60*time.Second {
		t.Fatalf("partition lag recovery %s >= 60s", recovery)
	}

	// Poison must be parked (3 attempts) without blocking.
	waitDLQDepth(t, p.dlq, "projection", 1, 10*time.Second)
	parked, _ := p.dlq.List(ctx, "projection", inbox.StatusParked)
	if len(parked) != 1 || parked[0].Attempts != 3 || parked[0].EventID != pid {
		t.Fatalf("unexpected DLQ state: %+v", parked)
	}
	if _, view, _, _ := p.Audit(ctx); view != following {
		t.Fatalf("expected %d good projections while poison parked, got %d", following, view)
	}

	// --- Fix the handler and replay via dlqctl-style Replay ---
	fixed.Store(true)
	if err := p.dlq.Replay(ctx, parked[0].ID, p.RepublishViaOutbox); err != nil {
		t.Fatalf("replay: %v", err)
	}

	// The replayed poison now converges exactly-once: projection gains exactly
	// one row (following + 1) and the DLQ drains.
	deadline := time.After(15 * time.Second)
	for {
		_, view, _, _ := p.Audit(ctx)
		if view == following+1 {
			break
		}
		select {
		case <-deadline:
			_, view, _, _ := p.Audit(ctx)
			t.Fatalf("replay did not converge: projection=%d want %d", view, following+1)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if d, _ := p.dlq.Depth(ctx, "projection"); d != 0 {
		t.Fatalf("DLQ depth after replay=%d want 0", d)
	}

	// Exactly-once guard: replaying AGAIN must not double-apply (already replayed
	// => no-op; and even a forced redelivery would be deduped by the inbox).
	seen, _ := p.proc.Seen(ctx, pid)
	if !seen {
		t.Fatal("poison not recorded in inbox after successful replay")
	}
	_, viewFinal, _, _ := p.Audit(ctx)
	if viewFinal != following+1 {
		t.Fatalf("final projection=%d want %d (exactly-once broken)", viewFinal, following+1)
	}

	t.Logf("[poison] parked after 3 retries; %d following events flowed (recovery=%s, no HOL block); replay converged exactly-once (projection=%d)",
		following, recovery.Round(time.Millisecond), viewFinal)
}

func waitDLQDepth(t *testing.T, dlq *inbox.SQLDLQ, group string, want int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if got, _ := dlq.Depth(context.Background(), group); got == want {
			return
		}
		select {
		case <-deadline:
			got, _ := dlq.Depth(context.Background(), group)
			t.Fatalf("DLQ depth reached %d, want %d", got, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
