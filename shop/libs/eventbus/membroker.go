package eventbus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// DefaultPartitions is the MemBroker partition count per topic. It bounds the
// consume parallelism (one worker goroutine per partition per subscription).
const DefaultPartitions = 16

// MemBroker is the in-process Broker standing in for the per-cell Kafka cluster
// (D5). Each topic is a fixed set of append-only partition logs; a key hashes
// to a stable partition (ordered-per-key). Consumer groups keep independent
// cursors; distinct partitions are consumed concurrently. It is safe for
// concurrent Publish/Subscribe and is the durable-enough backbone the soak,
// dedupe and poison criteria run against.
//
// A file-backed log or a real KafkaBroker is a drop-in behind the Broker
// interface; MemBroker keeps the append-only-log + per-group-cursor shape so
// that swap is mechanical.
type MemBroker struct {
	partitions int
	validate   bool

	mu     sync.RWMutex
	topics map[string]*topicLog
	closed bool

	published atomic.Int64 // total messages accepted (audit)
}

// MemOption configures a MemBroker.
type MemOption func(*MemBroker)

// WithPartitions sets the per-topic partition count (default DefaultPartitions).
func WithPartitions(n int) MemOption {
	return func(b *MemBroker) {
		if n > 0 {
			b.partitions = n
		}
	}
}

// WithPublishValidation makes Publish reject any message whose Raw fails
// ValidateEnvelope. Off by default because the outbox validates once at
// ingress; unit tests turn it on to prove the bus enforces the contract too.
func WithPublishValidation(on bool) MemOption {
	return func(b *MemBroker) { b.validate = on }
}

// NewMemBroker builds an in-process broker.
func NewMemBroker(opts ...MemOption) *MemBroker {
	b := &MemBroker{partitions: DefaultPartitions, topics: map[string]*topicLog{}}
	for _, o := range opts {
		o(b)
	}
	return b
}

// PublishedCount is the total number of messages the broker accepted (audit).
func (b *MemBroker) PublishedCount() int64 { return b.published.Load() }

func (b *MemBroker) topic(name string) *topicLog {
	b.mu.RLock()
	t := b.topics[name]
	b.mu.RUnlock()
	if t != nil {
		return t
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t = b.topics[name]; t == nil {
		t = newTopicLog(b.partitions)
		b.topics[name] = t
	}
	return t
}

// Publish appends messages to their partitions (ordered per key) and wakes
// consumers. At-least-once: an accepted message stays in the log until every
// group's cursor passes it.
func (b *MemBroker) Publish(ctx context.Context, msgs ...Message) error {
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrBrokerClosed
	}
	for i := range msgs {
		m := msgs[i]
		if b.validate {
			if err := ValidateEnvelope(m.Raw); err != nil {
				return fmt.Errorf("eventbus: refused message on %q: %w", m.Topic, err)
			}
		}
		t := b.topic(m.Topic)
		p := partitionFor(m.Key, b.partitions)
		m.Partition = p
		t.parts[p].append(m)
		b.published.Add(1)
	}
	return nil
}

// Subscribe starts a consumer: one worker per partition, each draining its
// partition in order with retry-then-park (D22, no head-of-line block).
func (b *MemBroker) Subscribe(cfg SubscribeConfig, h Handler) (Subscription, error) {
	if cfg.Topic == "" || cfg.Group == "" {
		return nil, fmt.Errorf("eventbus: Subscribe needs Topic and Group")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	t := b.topic(cfg.Topic)
	ctx, cancel := context.WithCancel(context.Background())
	s := &memSub{cancel: cancel}
	for p := 0; p < b.partitions; p++ {
		s.wg.Add(1)
		go s.runPartition(ctx, t.parts[p], cfg, h)
	}
	return s, nil
}

// Close stops the broker; in-flight Publish calls that raced will see
// ErrBrokerClosed.
func (b *MemBroker) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

// --- topic / partition logs ---

type topicLog struct {
	parts []*partitionLog
}

func newTopicLog(n int) *topicLog {
	t := &topicLog{parts: make([]*partitionLog, n)}
	for i := range t.parts {
		t.parts[i] = &partitionLog{}
		t.parts[i].cond = sync.NewCond(&t.parts[i].mu)
	}
	return t
}

type partitionLog struct {
	mu   sync.Mutex
	cond *sync.Cond
	log  []Message
}

func (pl *partitionLog) append(m Message) {
	pl.mu.Lock()
	m.Offset = int64(len(pl.log))
	pl.log = append(pl.log, m)
	pl.mu.Unlock()
	pl.cond.Broadcast()
}

// waitFor returns the message at offset once available, or ok=false if ctx is
// done. It blocks on the cond so there is no polling.
func (pl *partitionLog) waitFor(ctx context.Context, offset int64) (Message, bool) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for int64(len(pl.log)) <= offset {
		if ctx.Err() != nil {
			return Message{}, false
		}
		// Wake periodically so ctx cancellation is observed even without a new
		// append. A watcher goroutine broadcasts on cancel (see runPartition).
		pl.cond.Wait()
	}
	return pl.log[offset], true
}

// --- subscription / partition worker ---

type memSub struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *memSub) Close() error {
	s.cancel()
	s.wg.Wait()
	return nil
}

func (s *memSub) runPartition(ctx context.Context, pl *partitionLog, cfg SubscribeConfig, h Handler) {
	defer s.wg.Done()
	// Ensure a blocked waitFor wakes when the subscription is cancelled.
	go func() {
		<-ctx.Done()
		pl.cond.Broadcast()
	}()

	var offset int64
	for {
		if ctx.Err() != nil {
			return
		}
		msg, ok := pl.waitFor(ctx, offset)
		if !ok {
			return
		}
		s.deliver(ctx, msg, cfg, h)
		offset++ // advance cursor whether acked or parked — no head-of-line block
	}
}

func (s *memSub) deliver(ctx context.Context, msg Message, cfg SubscribeConfig, h Handler) {
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		lastErr = h(ctx, msg)
		if lastErr == nil {
			return // acked
		}
		if cfg.Backoff != nil && attempt < cfg.MaxAttempts {
			cfg.Backoff(attempt)
		}
	}
	// Exhausted retries — park to the DLQ and let the partition advance.
	if cfg.DLQ != nil {
		_ = cfg.DLQ.Park(ctx, msg, cfg.Group, cfg.MaxAttempts, lastErr)
	}
}
