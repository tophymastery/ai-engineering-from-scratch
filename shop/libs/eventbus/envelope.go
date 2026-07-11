package eventbus

import (
	"encoding/json"
	"time"
)

// Envelope is the 02 §4.3 event envelope every message on the bus carries.
// It is the wire contract shared by producers (via libs/outbox) and consumers
// (via libs/inbox). event_id is the dedupe key in the consumer inbox.
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	OccurredAt    string          `json:"occurred_at"`
	TraceID       string          `json:"trace_id"`
	Aggregate     Aggregate       `json:"aggregate"`
	SchemaVersion int             `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

// Aggregate identifies the entity an event is about. id is a prefixed ULID and
// is never PII (D3). type+id form the natural partition key.
type Aggregate struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Region string `json:"region,omitempty"`
}

// PartitionKey is the bus key for an envelope: the aggregate id (D5 keys topics
// by aggregate_id alone; the region prefix from 01 §3 is redundant once
// clusters are per-cell). One aggregate's events stay ordered in one partition.
func (e Envelope) PartitionKey() string { return e.Aggregate.ID }

// Marshal renders the envelope to canonical JSON wire bytes.
func (e Envelope) Marshal() ([]byte, error) { return json.Marshal(e) }

// UnmarshalEnvelope parses wire bytes back into an Envelope.
func UnmarshalEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	err := json.Unmarshal(b, &e)
	return e, err
}

// NewEnvelope is a small constructor used by the reference service and tests.
// occurred_at defaults to now (RFC 3339 UTC) when zero.
func NewEnvelope(eventID, eventType, traceID string, agg Aggregate, schemaVersion int, payload any, occurredAt time.Time) (Envelope, error) {
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		EventID:       eventID,
		EventType:     eventType,
		OccurredAt:    occurredAt.UTC().Format(time.RFC3339Nano),
		TraceID:       traceID,
		Aggregate:     agg,
		SchemaVersion: schemaVersion,
		Payload:       raw,
	}, nil
}
