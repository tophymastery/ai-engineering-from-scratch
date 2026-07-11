package inbox

import (
	"context"
	"sync"

	"github.com/shop-platform/shop/libs/eventbus"
)

// MemProcessor is an in-memory exactly-once inbox — the throughput stand-in for
// the high-rate soak, where a single-writer SQLite inbox would be the bottleneck
// rather than the backbone under test (documented adaptation in VERIFICATION).
// It gives the SAME contract as Processor.Process: the first delivery of an
// event_id applies; every redelivery is a no-op. Production uses the SQL
// Processor (durable, survives restart).
type MemProcessor struct {
	group string
	mu    sync.Mutex
	seen  map[string]struct{}
}

// NewMemProcessor builds an in-memory exactly-once inbox for a group.
func NewMemProcessor(group string) *MemProcessor {
	return &MemProcessor{group: group, seen: map[string]struct{}{}}
}

// Process applies the effect exactly once per event_id. effect runs only on the
// first delivery; the dedupe decision and effect are atomic under the lock, so
// concurrent redeliveries collapse to one effect (mirrors UNIQUE(event_id)).
func (m *MemProcessor) Process(_ context.Context, msg eventbus.Message, effect func() error) (applied bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.seen[msg.Envelope.EventID]; dup {
		return false, nil
	}
	if effect != nil {
		if err := effect(); err != nil {
			return false, err // not recorded — a retry may still apply
		}
	}
	m.seen[msg.Envelope.EventID] = struct{}{}
	return true, nil
}

// Count is the number of distinct events applied (audit).
func (m *MemProcessor) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}
