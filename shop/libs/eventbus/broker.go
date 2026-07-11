package eventbus

import (
	"context"
	"errors"
	"hash/fnv"
)

// Message is one record on the bus. Raw is the marshaled 02 §4.3 envelope;
// Envelope is its decoded form (carried alongside so consumers/relays avoid a
// re-parse). Partition/Offset are assigned by the broker at publish time.
type Message struct {
	Topic     string
	Key       string // partition key — the aggregate id (D5)
	Envelope  Envelope
	Raw       []byte
	Partition int
	Offset    int64
}

// NewMessage builds a Message for a topic from an envelope, defaulting the
// partition Key to the envelope's aggregate id.
func NewMessage(topic string, env Envelope) (Message, error) {
	raw, err := env.Marshal()
	if err != nil {
		return Message{}, err
	}
	return Message{Topic: topic, Key: env.PartitionKey(), Envelope: env, Raw: raw}, nil
}

// Handler processes a delivered message. Returning nil acks (advances the
// cursor); returning an error triggers redelivery, and after MaxAttempts the
// message is parked to the DLQ. Handlers must be safe to call more than once
// (at-least-once) — libs/inbox.Process gives them exactly-once effect.
type Handler func(ctx context.Context, msg Message) error

// Publisher publishes messages with ordered, partitioned, at-least-once
// semantics. Messages sharing a Key preserve publish order.
type Publisher interface {
	Publish(ctx context.Context, msgs ...Message) error
}

// Consumer subscribes a handler to a topic under a consumer group. Each group
// has independent cursors; distinct partitions are consumed concurrently.
type Consumer interface {
	Subscribe(cfg SubscribeConfig, h Handler) (Subscription, error)
}

// Broker is a full bus: a Publisher and a Consumer over a set of topics. Both
// MemBroker (here) and a future KafkaBroker satisfy it.
type Broker interface {
	Publisher
	Consumer
}

// SubscribeConfig configures one subscription.
type SubscribeConfig struct {
	Topic string
	Group string // consumer group — DLQ + cursors are per group (D22)
	// MaxAttempts is the number of handler tries before parking to the DLQ.
	// Zero means DefaultMaxAttempts (3, per S-T6).
	MaxAttempts int
	// DLQ receives parked messages. Nil is allowed only if MaxAttempts is 1 and
	// the caller accepts drops; production always wires a durable DLQSink.
	DLQ DLQSink
	// Backoff between handler retries within a partition. Zero = no delay.
	Backoff func(attempt int) // optional sleep hook; nil = immediate retry
}

// DefaultMaxAttempts is the S-T6 park-after-N-failures threshold.
const DefaultMaxAttempts = 3

// Subscription is a running consumer; Close stops its partition workers.
type Subscription interface {
	Close() error
}

// DLQSink parks a message that exhausted its retries. Implementations are
// durable (libs/inbox provides a SQL-backed one); eventbus ships an in-memory
// MemDLQ for its own tests. Park MUST NOT block the caller for long — the
// partition worker calls it inline before advancing its cursor.
type DLQSink interface {
	Park(ctx context.Context, msg Message, group string, attempts int, cause error) error
}

// ErrBrokerClosed is returned by Publish after the broker is closed.
var ErrBrokerClosed = errors.New("eventbus: broker closed")

// partitionFor maps a key to a partition via FNV-1a hash, giving stable
// key→partition assignment (the Kafka default-partitioner shape).
func partitionFor(key string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}
