package eventbus

import (
	"context"
	"sync"
	"time"
)

// ParkedMessage is a message the consumer gave up on (D22): the original
// message plus the failure context needed to inspect and replay it.
type ParkedMessage struct {
	Message  Message
	Group    string
	Attempts int
	Cause    string
	ParkedAt time.Time
}

// MemDLQ is an in-memory DLQSink for eventbus's own tests. Production consumers
// wire the SQL-backed DLQ from libs/inbox (durable, dlqctl-inspectable).
type MemDLQ struct {
	mu     sync.Mutex
	parked []ParkedMessage
}

// NewMemDLQ builds an empty in-memory DLQ.
func NewMemDLQ() *MemDLQ { return &MemDLQ{} }

// Park records a parked message.
func (d *MemDLQ) Park(_ context.Context, msg Message, group string, attempts int, cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := ""
	if cause != nil {
		c = cause.Error()
	}
	d.parked = append(d.parked, ParkedMessage{
		Message: msg, Group: group, Attempts: attempts, Cause: c, ParkedAt: time.Now().UTC(),
	})
	return nil
}

// List returns a snapshot of parked messages.
func (d *MemDLQ) List() []ParkedMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]ParkedMessage, len(d.parked))
	copy(out, d.parked)
	return out
}

// Depth is the current DLQ depth (the alert metric of interest).
func (d *MemDLQ) Depth() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.parked)
}
