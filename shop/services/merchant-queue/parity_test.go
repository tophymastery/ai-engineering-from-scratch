package main

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// buildOrderStream generates a canonical (per-order-ordered) event stream for n
// orders across m merchants: created → paid → (accepted | cancelled | left
// PENDING | dispatched). Returns the canonical events (for the reference fold)
// and a shuffled+duplicated delivery order (to stress LWW + exactly-once).
func buildOrderStream(n, m int, seed int64) (canonical []projectedEvent, delivery []projectedEvent) {
	rng := rand.New(rand.NewSource(seed))
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		oid := fmt.Sprintf("ord_%07d", i)
		mid := fmt.Sprintf("mer_%04d", i%m)
		at := base.Add(time.Duration(i) * time.Second)
		add := func(id, typ, merch string, off time.Duration) {
			phase, state, _ := phaseFor(typ)
			canonical = append(canonical, projectedEvent{
				EventID: id, OrderID: oid, MerchantID: merch,
				EventType: typ, Phase: phase, State: state, OccurredAt: at.Add(off),
				RawPayload: fmt.Sprintf(`{"order_id":%q,"merchant_id":%q}`, oid, merch),
			})
		}
		add("evt_c_"+oid, TopicOrderCreated, "", 0)
		add("evt_p_"+oid, TopicOrderPaid, mid, 3*time.Second)
		switch i % 4 {
		case 0, 1:
			add("evt_a_"+oid, TopicOrderAccepted, mid, 10*time.Second)
		case 2:
			add("evt_x_"+oid, TopicOrderCancelled, mid, 10*time.Second)
		case 3:
			// left PENDING (no further event)
		}
	}
	// Delivery order: shuffle, then duplicate ~10% of events (redelivery stress).
	delivery = append(delivery, canonical...)
	rng.Shuffle(len(delivery), func(a, b int) { delivery[a], delivery[b] = delivery[b], delivery[a] })
	dupes := make([]projectedEvent, 0, len(delivery)/10)
	for i, e := range delivery {
		if i%10 == 0 {
			dupes = append(dupes, e)
		}
	}
	delivery = append(delivery, dupes...)
	rng.Shuffle(len(delivery), func(a, b int) { delivery[a], delivery[b] = delivery[b], delivery[a] })
	return canonical, delivery
}

func deliver(t *testing.T, srv *server, ev projectedEvent) {
	t.Helper()
	extra := map[string]any{}
	env, err := makeOrderEnvelope(ev.EventID, ev.EventType, ev.OrderID, ev.MerchantID, "bkk", extra, ev.OccurredAt)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if _, err := srv.pr.InjectEnvelope(context.Background(), env); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

// TestProjectionParity10k: replay 10k orders' events (shuffled + duplicated) and
// assert the projected read model equals an INDEPENDENT reference fold — 100%
// parity — AND that a full + largest-cell rebuild from the event log reproduces
// it exactly. Runs under -race (real 10k reconcile).
func TestProjectionParity10k(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	const N, M = 10000, 300

	canonical, delivery := buildOrderStream(N, M, 42)
	start := time.Now()
	for _, ev := range delivery {
		deliver(t, srv, ev)
	}
	t.Logf("delivered %d events (%d canonical + duplicates) for %d orders in %v", len(delivery), len(canonical), N, time.Since(start))

	// 1. Parity vs the independent reference fold.
	want := refFoldState(canonical)
	got, err := srv.st.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(got) != N {
		t.Fatalf("projected %d orders, want %d", len(got), N)
	}
	mism := 0
	for id, w := range want {
		g, ok := got[id]
		if !ok || g.QueueState != w.QueueState || g.MerchantID != w.MerchantID || g.Shard != w.Shard || g.Cell != w.Cell {
			mism++
			if mism <= 5 {
				t.Errorf("parity mismatch %s: got %+v want state=%s merchant=%s shard=%d cell=%d", id, g, w.QueueState, w.MerchantID, w.Shard, w.Cell)
			}
		}
	}
	if mism != 0 {
		t.Fatalf("projection parity: %d/%d orders mismatched (want 0)", mism, N)
	}
	t.Logf("PROJECTION PARITY: %d/%d orders match the reference fold exactly (100%%)", N, N)

	// 2. Full rebuild from the log == live model.
	full, err := srv.st.Rebuild(ctx, -1, testBase)
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	if !full.ParityOK || full.Mismatches != 0 {
		t.Fatalf("full rebuild parity: mismatches=%d", full.Mismatches)
	}
	t.Logf("FULL REBUILD: %d orders, %d events replayed from the log, parity_ok=%v (%v)", full.Orders, full.Events, full.ParityOK, full.Duration)

	// 3. Rebuild of the LARGEST cell == live model for that cell.
	counts, _ := srv.st.cellCounts(ctx)
	largest, largestN := -1, -1
	for cell, c := range counts {
		if c > largestN {
			largest, largestN = cell, c
		}
	}
	cellRes, err := srv.st.Rebuild(ctx, largest, testBase)
	if err != nil {
		t.Fatalf("cell rebuild: %v", err)
	}
	if !cellRes.ParityOK || cellRes.Mismatches != 0 {
		t.Fatalf("largest-cell rebuild parity: mismatches=%d", cellRes.Mismatches)
	}
	t.Logf("LARGEST-CELL REBUILD: cell=%d orders=%d events=%d parity_ok=%v (%v)", cellRes.Cell, cellRes.Orders, cellRes.Events, cellRes.ParityOK, cellRes.Duration)
}
