package main

import (
	"context"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/sharding"
)

// TestProjectionLifecycle: order.created→paid→accepted projects the queue row
// forward; merchant_id + shard + cell are set (D11) from order.paid; the state
// tracks the lifecycle.
func TestProjectionLifecycle(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	oid, mid := "ord_life", "mer_life_42"

	srv.injectEvent(t, "e1", TopicOrderCreated, oid, "", testBase, map[string]any{"customer_id": "usr_1", "total": map[string]any{"amount": 42550, "currency": "THB"}})
	row, ok, _ := srv.st.getRow(ctx, oid)
	if !ok || row.QueueState != StateCreated {
		t.Fatalf("after created: state=%q ok=%v", row.QueueState, ok)
	}
	if row.MerchantID != "" {
		t.Fatalf("merchant should be unknown before order.paid, got %q", row.MerchantID)
	}

	srv.injectEvent(t, "e2", TopicOrderPaid, oid, mid, testBase.Add(3*time.Second), map[string]any{"paid_at": testBase.Format(time.RFC3339)})
	row, _, _ = srv.st.getRow(ctx, oid)
	if row.QueueState != StatePending {
		t.Fatalf("after paid: state=%q, want PENDING", row.QueueState)
	}
	// D11 sharding by merchant_id: shard + cell match the routing primitive.
	if row.MerchantID != mid || row.Shard != sharding.LogicalShard(mid) || row.Cell != sharding.LogicalShard(mid)%NumCells {
		t.Fatalf("sharding wrong: merchant=%q shard=%d cell=%d (want shard=%d cell=%d)",
			row.MerchantID, row.Shard, row.Cell, sharding.LogicalShard(mid), sharding.LogicalShard(mid)%NumCells)
	}

	srv.injectEvent(t, "e3", TopicOrderAccepted, oid, mid, testBase.Add(10*time.Second), nil)
	row, _, _ = srv.st.getRow(ctx, oid)
	if row.QueueState != StateAccepted {
		t.Fatalf("after accepted: state=%q, want ACCEPTED", row.QueueState)
	}
}

// TestProjectionLWWOutOfOrder: events delivered OUT OF ORDER (accepted before
// paid before created) still converge to ACCEPTED with merchant backfilled — the
// LWW forward-only rule ignores the stale earlier-phase events.
func TestProjectionLWWOutOfOrder(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	oid, mid := "ord_ooo", "mer_ooo"

	// accepted arrives FIRST.
	srv.injectEvent(t, "a", TopicOrderAccepted, oid, mid, testBase.Add(10*time.Second), nil)
	// then paid (earlier phase) — must NOT downgrade.
	srv.injectEvent(t, "p", TopicOrderPaid, oid, mid, testBase.Add(3*time.Second), map[string]any{"paid_at": testBase.Format(time.RFC3339)})
	// then created (earliest) — must NOT downgrade.
	srv.injectEvent(t, "c", TopicOrderCreated, oid, "", testBase, nil)

	row, _, _ := srv.st.getRow(ctx, oid)
	if row.QueueState != StateAccepted {
		t.Fatalf("out-of-order converged to %q, want ACCEPTED", row.QueueState)
	}
	if row.MerchantID != mid {
		t.Fatalf("merchant backfill failed: %q", row.MerchantID)
	}
}

// TestCancelWinsTerminal: order.cancelled from PENDING is terminal.
func TestCancelWinsTerminal(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	oid, mid := "ord_can", "mer_can"
	srv.injectEvent(t, "p", TopicOrderPaid, oid, mid, testBase, map[string]any{"paid_at": testBase.Format(time.RFC3339)})
	srv.injectEvent(t, "x", TopicOrderCancelled, oid, mid, testBase.Add(time.Minute), nil)
	row, _, _ := srv.st.getRow(ctx, oid)
	if row.QueueState != StateCancelled {
		t.Fatalf("state=%q, want CANCELLED", row.QueueState)
	}
	// A late accepted (lower phase than cancelled=99) must NOT resurrect it.
	srv.injectEvent(t, "a", TopicOrderAccepted, oid, mid, testBase.Add(2*time.Minute), nil)
	row, _, _ = srv.st.getRow(ctx, oid)
	if row.QueueState != StateCancelled {
		t.Fatalf("cancelled resurrected to %q", row.QueueState)
	}
}

// TestShardDistributionUniform: across many merchants the cell assignment is
// balanced (the murmur3-finalized routing avalanches merchant_id; D11/D6).
func TestShardDistributionUniform(t *testing.T) {
	counts := make([]int, NumCells)
	const N = 20000
	for i := 0; i < N; i++ {
		mid := "mer_dist_" + itoa(i)
		counts[cellFor(mid)]++
	}
	mean := float64(N) / float64(NumCells)
	for c, n := range counts {
		dev := float64(n)/mean - 1.0
		if dev < -0.05 || dev > 0.05 {
			t.Fatalf("cell %d has %d rows (%.1f%% off the %.0f mean) — distribution not uniform", c, n, dev*100, mean)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
