package rank

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// signalEnvelope builds a ranking.signal event envelope.
func signalEnvelope(eventID, merchantID, signalType string, weight float64) eventbus.Envelope {
	payload, _ := json.Marshal(signalPayload{MerchantID: merchantID, SignalType: signalType, Weight: weight})
	return eventbus.Envelope{
		EventID:       eventID,
		EventType:     TopicRankingSignal,
		OccurredAt:    time.Now().UTC().Format(time.RFC3339Nano),
		TraceID:       "t_test",
		Aggregate:     eventbus.Aggregate{Type: "merchant", ID: merchantID, Region: "bkk"},
		SchemaVersion: 1,
		Payload:       payload,
	}
}

// waitInbox polls the consumer until it has applied want distinct events (bus
// delivery is async — one goroutine per partition), failing after a bound.
func waitInbox(t *testing.T, n *Node, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n.Consumer.InboxCount() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d applied signals (got %d)", want, n.Consumer.InboxCount())
}

// TestFeatureStore_FromEvents is the V-T5 "event-fed feature store" property: the
// re-ranking features are populated ENTIRELY by consuming ranking.signal events
// through the REAL eventbus + inbox, and re-rank order changes once they land.
func TestFeatureStore_FromEvents(t *testing.T) {
	n := NewNode("ranking-signals-test", Options{})
	defer n.Close()

	// Before any events, A has no popularity: ML ranks higher-rated B first.
	cands := []Candidate{candB(), candA()}
	pre, _ := n.Ranker.Rank(context.Background(), cands, 10, true)
	if pre[0].StoreID != "mer_b_highrated" {
		t.Fatalf("pre-signal ML: expected B first (A has no features), got %s", pre[0].StoreID)
	}

	// Stream 12 ORDER signals for A through the bus (exactly-once via inbox).
	for i := 0; i < 12; i++ {
		env := signalEnvelope(fmt.Sprintf("evt_order_%02d", i), "mer_a_popular", SignalOrder, 1)
		if err := n.Publish(context.Background(), env); err != nil {
			t.Fatalf("publish signal: %v", err)
		}
	}
	waitInbox(t, n, 12)

	if got := n.Features.Merchants(); got != 1 {
		t.Fatalf("feature store should track 1 merchant, got %d", got)
	}
	if pop := n.Features.Popularity("mer_a_popular"); pop <= 0 {
		t.Fatalf("popularity feature should be > 0 after order signals, got %f", pop)
	}

	// After the events, ML re-rank promotes the now-popular A above higher-rated B.
	post, _ := n.Ranker.Rank(context.Background(), cands, 10, true)
	if post[0].StoreID != "mer_a_popular" {
		t.Fatalf("post-signal ML: expected popular A promoted to first, got %s", post[0].StoreID)
	}
}

// TestConsumer_ExactlyOnce proves a redelivered signal never double-counts (the
// inbox exactly-once effect) — otherwise popularity would drift under
// at-least-once delivery.
func TestConsumer_ExactlyOnce(t *testing.T) {
	feats := NewFeatureStore()
	c := NewConsumer(feats, "exactly-once-test")
	env := signalEnvelope("evt_dup_1", "mer_x", SignalOrder, 5)
	msg, _ := eventbus.NewMessage(TopicRankingSignal, env)

	for i := 0; i < 10; i++ { // 10 deliveries of the SAME event_id
		if err := c.Handle(context.Background(), msg); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if c.InboxCount() != 1 {
		t.Fatalf("exactly-once: expected 1 applied event, got %d", c.InboxCount())
	}
	// Orders folded exactly once (weight 5), not 50.
	f := feats.get("mer_x")
	if f.Orders != 5 {
		t.Fatalf("exactly-once effect: expected Orders=5 (single apply), got %v", f.Orders)
	}
}

// TestFeatureStore_CTR proves the click/impression conversion signal is event-fed
// and reflected in the score.
func TestFeatureStore_CTR(t *testing.T) {
	feats := NewFeatureStore()
	feats.Apply("mer_ctr", SignalImpression, 100)
	feats.Apply("mer_ctr", SignalClick, 40)
	f := feats.get("mer_ctr")
	if f.ctr() != 0.4 {
		t.Fatalf("ctr: expected 0.4, got %f", f.ctr())
	}
}
