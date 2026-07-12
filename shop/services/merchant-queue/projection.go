package main

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// projection.go — the CQRS projection that feeds the merchant incoming-order read
// model from order.* events (D7). Consumption is EXACTLY-ONCE via the durable SQL
// inbox (libs/inbox): a redelivered event_id collides on the inbox unique key and
// is a no-op, so a Kafka redelivery produces at most one projected effect. The
// model apply AND the append to the rebuild log run on the inbox's transaction,
// so the read-model row, the inbox dedupe row, and the log row commit atomically
// (S-T6 exactly-once). LWW forward-only ordering (store.applyModelTx) means
// out-of-order delivery across the salted merchant partitions (D11) still
// converges.

// Projection is the order.* → read-model consumer for a group.
type Projection struct {
	st    *store
	clock Clock

	fresh freshness // paid→visible latency recorder (dashboard datum)
}

func newProjection(st *store, clock Clock) *Projection {
	return &Projection{st: st, clock: clock}
}

// Handle is the eventbus.Handler. The inbox gives exactly-once effect; the fold
// apply + log append run on the inbox tx so they commit atomically with the
// dedupe row. A topic the queue does not project is a benign no-op (still
// dedupe-committed so it is not re-parked).
func (pr *Projection) Handle(ctx context.Context, msg eventbus.Message) error {
	env := msg.Envelope
	p, err := decodeOrderPayload(env.Payload)
	if err != nil {
		return err
	}
	if p.OrderID == "" {
		p.OrderID = env.Aggregate.ID
	}
	ev, ok := project(env.EventID, env.EventType, parseTime(env.OccurredAt), p, string(env.Payload))
	if !ok {
		return nil // not a projected topic
	}
	now := pr.clock.Now()
	start := time.Now()
	applied, err := pr.st.inbx.Process(ctx, msg, func(ctx context.Context, tx *sql.Tx) error {
		if err := pr.st.applyModelTx(ctx, tx, ev, now); err != nil {
			return err
		}
		return pr.st.logEventTx(ctx, tx, ev)
	})
	if err != nil {
		return err
	}
	// Record freshness for order.paid (the "queue freshness from order.paid" SLO):
	// the wall time from starting the apply to the row being visible. Only the
	// first (non-duplicate) delivery counts.
	if applied && ev.EventType == TopicOrderPaid {
		pr.fresh.record(time.Since(start))
	}
	return nil
}

// InjectEnvelope routes a raw envelope through the projection (the E2E stub-event
// delivery path — mirrors order's /v1/order-events / search's /v1/index/events —
// and the redelivery-fixture test entry point). Returns the eventbus.Message it
// built so tests can redeliver the SAME event_id.
func (pr *Projection) InjectEnvelope(ctx context.Context, env eventbus.Envelope) (eventbus.Message, error) {
	if env.EventID == "" {
		env.EventID = newToken("evt")
	}
	msg, err := eventbus.NewMessage(env.EventType, env)
	if err != nil {
		return eventbus.Message{}, err
	}
	return msg, pr.Handle(ctx, msg)
}

// freshness is a small concurrency-safe latency recorder for the paid→visible
// projection lag (the queue-freshness SLO datum surfaced on the dashboard).
type freshness struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (f *freshness) record(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, d)
}

// stats returns count + p50/p99 of the recorded projection lag.
func (f *freshness) stats() (n int, p50, p99 time.Duration) {
	f.mu.Lock()
	s := append([]time.Duration(nil), f.samples...)
	f.mu.Unlock()
	n = len(s)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	p50 = s[n*50/100]
	p99 = s[min(n*99/100, n-1)]
	return n, p50, p99
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
