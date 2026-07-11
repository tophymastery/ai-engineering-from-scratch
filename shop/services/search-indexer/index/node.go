package index

import (
	"context"

	"github.com/shop-platform/shop/libs/eventbus"
)

// Node bundles the search read model into a runnable unit: the Engine (the
// index), an in-process eventbus (the per-cell Kafka stand-in), and a Consumer
// subscribed to the three merchant topics. Both the search-indexer binary (which
// runs the consumer) and the search-query binary (which in this sandbox embeds
// the indexer, since two processes cannot share an in-memory OpenSearch) build a
// Node. In production these are two deployments over a shared per-cell OpenSearch;
// that store adaptation is disclosed in VERIFICATION.md §V-T4.
type Node struct {
	Engine   *Engine
	Broker   *eventbus.MemBroker
	Consumer *Consumer
	subs     []eventbus.Subscription
	dlq      *eventbus.MemDLQ
}

// NewNode wires an engine, a broker, and a consumer subscribed to menu.updated /
// store.status_changed / rating.updated under the given group.
func NewNode(group string, opt EngineOptions) *Node {
	eng := NewEngine(opt)
	broker := eventbus.NewMemBroker()
	cons := NewConsumer(eng, group)
	n := &Node{Engine: eng, Broker: broker, Consumer: cons, dlq: eventbus.NewMemDLQ()}
	for _, topic := range []string{TopicMenuUpdated, TopicStoreStatus, TopicRatingUpdated} {
		sub, err := broker.Subscribe(eventbus.SubscribeConfig{
			Topic: topic, Group: group, DLQ: n.dlq,
		}, cons.Handle)
		if err != nil {
			panic("search-indexer: subscribe " + topic + ": " + err.Error())
		}
		n.subs = append(n.subs, sub)
	}
	return n
}

// Publish appends an envelope to its topic on the in-process bus (used by the
// ingest HTTP endpoint and tests). Keyed by the salted merchant key so a chain
// merchant's document stream spreads across partitions (D11).
func (n *Node) Publish(ctx context.Context, topic string, env eventbus.Envelope, salt int) error {
	msg, err := eventbus.NewMessage(topic, env)
	if err != nil {
		return err
	}
	msg.Key = SaltedKey(env.Aggregate.ID, salt)
	return n.Broker.Publish(ctx, msg)
}

// Close stops the subscriptions and the engine's ingest workers.
func (n *Node) Close() {
	for _, s := range n.subs {
		_ = s.Close()
	}
	_ = n.Broker.Close()
	n.Engine.Close()
}
