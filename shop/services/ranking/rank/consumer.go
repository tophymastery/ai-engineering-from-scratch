package rank

import (
	"context"
	"encoding/json"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
)

// consumer.go — the event projection that FEEDS the ranking feature store. The
// ranking model is only as good as its features, and those features are event
// sourced: retrieval-order behaviour (impression / click / order signals) streams
// in on the `ranking.signal` topic and the consumer folds each signal into the
// per-merchant feature vector (features.go). Consumption goes through the
// established inbox pattern (libs/inbox) for EXACTLY-ONCE effect — a redelivered
// signal never double-counts a click or an order, so the popularity/CTR features
// are stable under at-least-once delivery.
//
// Signals are naturally last-write-agnostic running aggregates (each is an
// increment), so ordering across partitions does not matter; only exactly-once
// does, which the inbox provides.

// TopicRankingSignal is the retrieval-behaviour signal topic ranking consumes.
const TopicRankingSignal = "ranking.signal"

// signalPayload mirrors contracts/events/ranking.signal/v1.schema.json.
type signalPayload struct {
	MerchantID string  `json:"merchant_id"`
	SignalType string  `json:"signal_type"` // impression | click | order
	Weight     float64 `json:"weight"`
}

// Consumer projects `ranking.signal` events onto a FeatureStore with exactly-once
// effect (inbox). One Consumer per consumer group.
type Consumer struct {
	feats *FeatureStore
	inbox *inbox.MemProcessor
}

// NewConsumer builds a signal projection consumer for a group. The MemProcessor
// is the in-memory exactly-once inbox (the documented high-rate stand-in for the
// SQL inbox; identical first-apply/replay-noop contract).
func NewConsumer(feats *FeatureStore, group string) *Consumer {
	return &Consumer{feats: feats, inbox: inbox.NewMemProcessor(group)}
}

// Handle is the eventbus.Handler for the projection: exactly-once effect via the
// inbox (a redelivered event_id is a no-op), dispatch by event_type.
func (c *Consumer) Handle(ctx context.Context, msg eventbus.Message) error {
	_, err := c.inbox.Process(ctx, msg, func() error { return c.apply(msg.Envelope) })
	return err
}

func (c *Consumer) apply(env eventbus.Envelope) error {
	if env.EventType != TopicRankingSignal {
		return nil // ignore foreign topics defensively
	}
	var p signalPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return err
	}
	c.feats.Apply(p.MerchantID, p.SignalType, p.Weight)
	return nil
}

// InboxCount is the number of distinct signals applied (dedupe proof).
func (c *Consumer) InboxCount() int { return c.inbox.Count() }
