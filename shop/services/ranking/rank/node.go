package rank

import (
	"context"

	"github.com/shop-platform/shop/libs/eventbus"
)

// Node bundles the ranking read model into a runnable unit: the FeatureStore, the
// Ranker (ML + static + auto-fallback), an in-process eventbus (the per-cell Kafka
// stand-in), and a Consumer subscribed to the `ranking.signal` topic. The ranking
// binary builds a Node; production runs the same code over a real broker + online
// feature store (disclosed in VERIFICATION.md §V-T5).
type Node struct {
	Features *FeatureStore
	Ranker   *Ranker
	Broker   *eventbus.MemBroker
	Consumer *Consumer
	subs     []eventbus.Subscription
	dlq      *eventbus.MemDLQ
}

// NewNode wires a feature store, a ranker, a broker, and a signal consumer under
// the given group.
func NewNode(group string, opt Options) *Node {
	feats := NewFeatureStore()
	ranker := NewRanker(feats, opt)
	broker := eventbus.NewMemBroker()
	cons := NewConsumer(feats, group)
	n := &Node{Features: feats, Ranker: ranker, Broker: broker, Consumer: cons, dlq: eventbus.NewMemDLQ()}
	sub, err := broker.Subscribe(eventbus.SubscribeConfig{Topic: TopicRankingSignal, Group: group, DLQ: n.dlq}, cons.Handle)
	if err != nil {
		panic("ranking: subscribe " + TopicRankingSignal + ": " + err.Error())
	}
	n.subs = append(n.subs, sub)
	return n
}

// Publish appends an envelope to its topic on the in-process bus (used by the
// signal-ingest HTTP endpoint and tests). Keyed by the merchant aggregate id so a
// merchant's signal stream stays ordered in one partition.
func (n *Node) Publish(ctx context.Context, env eventbus.Envelope) error {
	msg, err := eventbus.NewMessage(env.EventType, env)
	if err != nil {
		return err
	}
	return n.Broker.Publish(ctx, msg)
}

// Close stops the subscriptions and the broker.
func (n *Node) Close() {
	for _, s := range n.subs {
		_ = s.Close()
	}
	_ = n.Broker.Close()
}
