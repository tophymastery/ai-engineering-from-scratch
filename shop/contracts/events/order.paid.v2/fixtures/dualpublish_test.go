package fixtures

import (
	"encoding/json"
	"testing"
)

// orderPaidFact is the single domain event the producer holds internally. During
// the D30 dual-publish window it is serialised into TWO wire shapes.
type orderPaidFact struct {
	eventID   string
	orderID   string
	paymentID string
	region    string
	amount    int
	currency  string
	tipAmount int
	paidAt    string
}

func (f orderPaidFact) envelope(eventType string, payload map[string]any, schemaVersion int) map[string]any {
	return map[string]any{
		"event_id":       f.eventID,
		"event_type":     eventType,
		"occurred_at":    f.paidAt,
		"trace_id":       "4bf92f3577b34da6a3ce929d0e0e4736",
		"aggregate":      map[string]any{"type": "order", "id": f.orderID, "region": f.region},
		"schema_version": schemaVersion,
		"payload":        payload,
	}
}

// emitV1 produces the legacy order.paid message (payload.total).
func (f orderPaidFact) emitV1() map[string]any {
	return f.envelope("order.paid", map[string]any{
		"order_id":   f.orderID,
		"payment_id": f.paymentID,
		"total":      map[string]any{"amount": f.amount, "currency": f.currency},
		"paid_at":    f.paidAt,
	}, 1)
}

// emitV2 produces the new order.paid.v2 message (payload.order_total + tip).
func (f orderPaidFact) emitV2() map[string]any {
	return f.envelope("order.paid.v2", map[string]any{
		"order_id":    f.orderID,
		"payment_id":  f.paymentID,
		"order_total": map[string]any{"amount": f.amount, "currency": f.currency},
		"tip":         map[string]any{"amount": f.tipAmount, "currency": f.currency},
		"paid_at":     f.paidAt,
	}, 2)
}

// roundtrip forces the message through JSON (numbers become float64, unknown-key
// checks are real) so validation exercises the true wire representation.
func roundtrip(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// TestDualPublish_BothGenerationsGreen is the DoD ".v2 dual-publish fixture =>
// both consumer generations green": ONE producer emits both topics; a gen-1
// consumer reads order.paid, a gen-2 consumer reads order.paid.v2; each message
// validates against its registry schema and each consumer extracts its own field.
func TestDualPublish_BothGenerationsGreen(t *testing.T) {
	v1Schema, err := LoadSchema("../../order.paid/v1.schema.json")
	if err != nil {
		t.Fatalf("load v1 schema: %v", err)
	}
	v2Schema, err := LoadSchema("../v2.schema.json")
	if err != nil {
		t.Fatalf("load v2 schema: %v", err)
	}

	fact := orderPaidFact{
		eventID: "evt_01H8XGPAID", orderID: "ord_01H8XGJ2Q", paymentID: "pay_01H8XGK5V",
		region: "bkk", amount: 42550, currency: "THB", tipAmount: 2000,
		paidAt: "2026-07-11T02:15:00Z",
	}

	// Producer dual-publishes.
	msgV1 := roundtrip(t, fact.emitV1())
	msgV2 := roundtrip(t, fact.emitV2())

	// Both messages are valid against their OWN registry schema.
	if v := Validate(v1Schema, msgV1); len(v) != 0 {
		t.Fatalf("gen-1 message invalid against order.paid/v1: %s", Pretty(v))
	}
	if v := Validate(v2Schema, msgV2); len(v) != 0 {
		t.Fatalf("gen-2 message invalid against order.paid.v2: %s", Pretty(v))
	}

	// Gen-1 consumer: reads order.paid, uses payload.total.
	got1 := consumeGen1(t, msgV1)
	if got1 != 42550 {
		t.Fatalf("gen-1 consumer read total=%d, want 42550", got1)
	}

	// Gen-2 consumer: reads order.paid.v2, uses payload.order_total + tip.
	total2, tip2 := consumeGen2(t, msgV2)
	if total2 != 42550 || tip2 != 2000 {
		t.Fatalf("gen-2 consumer read order_total=%d tip=%d, want 42550/2000", total2, tip2)
	}

	t.Logf("dual-publish GREEN: gen-1 total=%d (order.paid); gen-2 order_total=%d tip=%d (order.paid.v2)", got1, total2, tip2)
}

// TestDualPublish_ShapesAreGenuinelyIncompatible proves WHY this needed a new
// topic (D30): each generation's message FAILS the other generation's schema —
// the total->order_total rename is not additive, so it could never have been an
// in-place edit of order.paid.
func TestDualPublish_ShapesAreGenuinelyIncompatible(t *testing.T) {
	v1Schema, _ := LoadSchema("../../order.paid/v1.schema.json")
	v2Schema, _ := LoadSchema("../v2.schema.json")
	fact := orderPaidFact{
		eventID: "evt_x", orderID: "ord_x", paymentID: "pay_x", region: "bkk",
		amount: 100, currency: "THB", tipAmount: 10, paidAt: "2026-07-11T02:15:00Z",
	}
	if v := Validate(v2Schema, roundtrip(t, fact.emitV1())); len(v) == 0 {
		t.Fatal("expected the v1 message to FAIL the v2 schema (rename proves incompatibility)")
	}
	if v := Validate(v1Schema, roundtrip(t, fact.emitV2())); len(v) == 0 {
		t.Fatal("expected the v2 message to FAIL the v1 schema (rename proves incompatibility)")
	}
}

func consumeGen1(t *testing.T, msg map[string]any) int {
	t.Helper()
	payload := msg["payload"].(map[string]any)
	total := payload["total"].(map[string]any)
	return int(total["amount"].(float64))
}

func consumeGen2(t *testing.T, msg map[string]any) (int, int) {
	t.Helper()
	payload := msg["payload"].(map[string]any)
	total := payload["order_total"].(map[string]any)
	tip := payload["tip"].(map[string]any)
	return int(total["amount"].(float64)), int(tip["amount"].(float64))
}
