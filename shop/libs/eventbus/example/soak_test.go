package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
	"github.com/shop-platform/shop/libs/outbox"
)

// soakSeconds is the soak window. The 2h duration in the S-T6 criterion is not
// feasible in this sandbox (VERIFICATION says so); the default keeps `go test`
// fast while still SUSTAINING >=10k events/s and holding lag p99 < 2s across a
// multi-second window. Set SOAK_SECONDS=60 for the recorded long run.
func soakSeconds() int {
	if v := os.Getenv("SOAK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}

// TestSoak drives outbox -> relay(CDC tail) -> bus -> inbox(exactly-once) at a
// paced high rate, drops outbox partitions DURING the run, and audits that
// published == consumed exactly-once with relay lag p99 < 2s and zero loss.
func TestSoak(t *testing.T) {
	dur := time.Duration(soakSeconds()) * time.Second
	const targetRate = 20000 // aggregate events/s (paced; must sustain >=10k/s)
	const producers = 4
	const batch = 20

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := outbox.NewMemStore()
	bus := eventbus.NewMemBroker(eventbus.WithPartitions(16))
	proc := inbox.NewMemProcessor("soak")
	lag := eventbus.NewLagRecorder(targetRate * (soakSeconds() + 2))
	relay := outbox.NewCDCTailRelay(store, bus, outbox.RelayConfig{Name: "soak", Lag: lag, Batch: 1000})

	var consumed atomic.Int64
	sub, err := bus.Subscribe(eventbus.SubscribeConfig{Topic: topicOrderPaid, Group: "soak"}, func(_ context.Context, m eventbus.Message) error {
		applied, _ := proc.Process(ctx, m, nil)
		if applied {
			consumed.Add(1)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	go relay.Run(ctx)

	// Partition-drop janitor: continuously age + drop the published tail so the
	// outbox stays memory-flat, proving partition drop DURING the soak is
	// loss-free (the guard refuses to drop anything past the relay cursor).
	var drops atomic.Int64
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				cursor, _ := store.LoadCursor(ctx, "soak")
				safe := cursor - 3000 // only age rows well behind the cursor
				if safe <= 0 {
					continue
				}
				if store.BackdateBelow(safe, "2000-01-01") > 0 {
					n, err := store.DropPublishedBefore(ctx, "soak", time.Now().UTC())
					if err != nil {
						t.Errorf("partition drop during soak failed (event loss risk): %v", err)
						return
					}
					drops.Add(int64(n))
				}
			}
		}
	}()

	// Paced producers.
	var produced atomic.Int64
	var ids ulid
	start := time.Now()
	end := start.Add(dur)
	interval := time.Duration(float64(time.Second) * float64(batch*producers) / float64(targetRate))
	var wg sync.WaitGroup
	for g := 0; g < producers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := time.Now()
			for time.Now().Before(end) {
				for i := 0; i < batch; i++ {
					eid := ids.next("evt")
					env, _ := eventbus.NewEnvelope(eid, topicOrderPaid, "tr", eventbus.Aggregate{Type: "order", ID: ids.next("ord")}, 3, map[string]any{"amount": 1}, time.Time{})
					if _, err := store.Append(topicOrderPaid, env); err != nil {
						t.Errorf("append: %v", err)
						return
					}
					produced.Add(1)
				}
				next = next.Add(interval)
				if d := time.Until(next); d > 0 {
					time.Sleep(d)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	total := produced.Load()

	// Drain: wait until the relay has published and the inbox consumed all.
	drainDeadline := time.After(30 * time.Second)
	for relay.Published() < total || consumed.Load() < total {
		select {
		case <-drainDeadline:
			t.Fatalf("drain timeout: produced=%d published=%d consumed=%d", total, relay.Published(), consumed.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-janitorDone

	rate := float64(total) / elapsed.Seconds()
	p99 := lag.Quantile(0.99)
	p999 := lag.Quantile(0.999)

	// --- Assertions (the S-T6 soak criteria) ---
	if rate < 10000 {
		t.Fatalf("sustained rate %.0f events/s < 10000", rate)
	}
	if p99 >= 2*time.Second {
		t.Fatalf("relay lag p99 %s >= 2s", p99)
	}
	if lag.Max() >= 2*time.Second {
		t.Fatalf("relay lag max %s >= 2s (must hold throughout)", lag.Max())
	}
	// Exactly-once audit: published == consumed == produced, unique effects.
	if relay.Published() != total {
		t.Fatalf("published %d != produced %d (loss/dup)", relay.Published(), total)
	}
	if consumed.Load() != total {
		t.Fatalf("consumed %d != produced %d (loss/dup)", consumed.Load(), total)
	}
	if int64(proc.Count()) != total {
		t.Fatalf("unique effects %d != produced %d (duplicate side effect)", proc.Count(), total)
	}
	if drops.Load() == 0 {
		t.Fatalf("no partition drop occurred during soak")
	}

	fmt.Printf("[soak] dur=%s produced=%d rate=%.0f/s lag_p99=%s lag_p999=%s lag_max=%s partition_drops=%d outbox_retained=%d\n",
		elapsed.Round(time.Millisecond), total, rate, p99, p999, lag.Max(), drops.Load(), store.Len())
}
