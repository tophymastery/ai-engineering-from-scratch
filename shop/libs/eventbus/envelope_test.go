package eventbus

import (
	"os"
	"testing"
	"time"
)

func TestValidateEnvelopeAccepts(t *testing.T) {
	env, _ := NewEnvelope("evt_1", "order.paid", "trace", Aggregate{Type: "order", ID: "ord_1", Region: "bkk"}, 3, map[string]any{"amount": 100}, time.Now())
	raw, _ := env.Marshal()
	if err := ValidateEnvelope(raw); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
}

func TestValidateEnvelopeRejects(t *testing.T) {
	cases := map[string]string{
		"missing event_id":      `{"event_type":"order.paid","occurred_at":"t","trace_id":"x","aggregate":{"type":"order","id":"o"},"schema_version":1,"payload":{}}`,
		"missing aggregate.id":  `{"event_id":"e","event_type":"order.paid","occurred_at":"t","trace_id":"x","aggregate":{"type":"order"},"schema_version":1,"payload":{}}`,
		"schema_version string": `{"event_id":"e","event_type":"order.paid","occurred_at":"t","trace_id":"x","aggregate":{"type":"order","id":"o"},"schema_version":"1","payload":{}}`,
		"payload not object":    `{"event_id":"e","event_type":"order.paid","occurred_at":"t","trace_id":"x","aggregate":{"type":"order","id":"o"},"schema_version":1,"payload":5}`,
	}
	for name, body := range cases {
		if err := ValidateEnvelope([]byte(body)); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// TestSchemaNoDrift pins the embedded envelope schema to the canonical contract
// file (S-T5). If the contract changes, this test fails until the embedded copy
// is refreshed — the bus can never validate against a stale schema.
func TestSchemaNoDrift(t *testing.T) {
	canonical, err := os.ReadFile("../../contracts/events/envelope.schema.json")
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	if string(canonical) != string(envelopeSchemaJSON) {
		t.Fatalf("embedded envelope.schema.json drifted from contracts/events/envelope.schema.json; re-copy it")
	}
}
