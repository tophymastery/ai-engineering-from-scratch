package main

import (
	"encoding/json"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// events.go — the order.* event vocabulary the merchant-queue projection consumes
// (02 §4.3), and the LWW ordering semantics. The read model is projected from the
// order aggregate's lifecycle events; because the salted/partitioned bus only
// guarantees per-aggregate ordering (D5/D11), every apply is LAST-WRITE-WINS by a
// monotonic lifecycle `phase` — an event only advances an order forward, so
// out-of-order or duplicate delivery converges to the same state.

// Consumed order.* topics (the queue is fed by the whole order lifecycle; only
// order.paid puts an order INTO the accept queue, but the later states move it
// OUT so the queue read stays accurate).
const (
	TopicOrderCreated   = "order.created"
	TopicOrderPaid      = "order.paid"
	TopicOrderAccepted  = "order.accepted"
	TopicOrderCancelled = "order.cancelled"
	TopicOrderDispatched = "order.dispatched"
	TopicOrderPickedUp  = "order.picked_up"
	TopicOrderDelivered = "order.delivered"
	TopicOrderSettled   = "order.settled"
)

// ConsumedTopics is the set of topics the projection subscribes to.
var ConsumedTopics = []string{
	TopicOrderCreated, TopicOrderPaid, TopicOrderAccepted, TopicOrderCancelled,
	TopicOrderDispatched, TopicOrderPickedUp, TopicOrderDelivered, TopicOrderSettled,
}

// Queue states (the read-model projection of the order lifecycle from the
// merchant's point of view). PENDING is "awaiting merchant accept" — the actual
// incoming-order queue.
const (
	StateCreated    = "CREATED"
	StatePending    = "PENDING" // order.paid → awaiting merchant accept (in the queue)
	StateAccepted   = "ACCEPTED"
	StateRejected   = "REJECTED"
	StateCancelled  = "CANCELLED"
	StateDispatched = "DISPATCHED"
	StatePickedUp   = "PICKED_UP"
	StateDelivered  = "DELIVERED"
	StateSettled    = "SETTLED"
)

// phaseFor maps an order.* event_type to its monotonic lifecycle phase and the
// queue state it projects to. Higher phase = later in the lifecycle. cancelled is
// terminal (phase 99) and wins over any non-terminal state. An unknown topic
// returns ok=false (forward-compat: ignore).
func phaseFor(eventType string) (phase int, state string, ok bool) {
	switch eventType {
	case TopicOrderCreated:
		return 1, StateCreated, true
	case TopicOrderPaid:
		return 2, StatePending, true
	case TopicOrderAccepted:
		return 3, StateAccepted, true
	case TopicOrderDispatched:
		return 4, StateDispatched, true
	case TopicOrderPickedUp:
		return 5, StatePickedUp, true
	case TopicOrderDelivered:
		return 6, StateDelivered, true
	case TopicOrderSettled:
		return 7, StateSettled, true
	case TopicOrderCancelled:
		return 99, StateCancelled, true
	}
	return 0, "", false
}

// orderPayload is the union of the order.* payload fields the projection reads.
// merchant_id is carried by order.paid (additive to order.paid/v1, D30) and
// order.accepted; order.created carries customer_id + total. Absent fields stay
// zero and are ignored (additive-only tolerance).
type orderPayload struct {
	OrderID    string   `json:"order_id"`
	MerchantID string   `json:"merchant_id"`
	CustomerID string   `json:"customer_id"`
	Total      *moneyJSON `json:"total"`
	PaidAt     string   `json:"paid_at"`
	AcceptedAt string   `json:"accepted_at"`
}

type moneyJSON struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func decodeOrderPayload(raw json.RawMessage) (orderPayload, error) {
	var p orderPayload
	if len(raw) == 0 {
		return p, nil
	}
	err := json.Unmarshal(raw, &p)
	return p, err
}

// parseTime parses an RFC3339[Nano] timestamp, returning the zero time on empty
// or unparseable input.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// makeOrderEnvelope builds an inbound order.* envelope (the E2E stub-event path +
// test fixtures). merchant_id/customer_id/total are placed in the payload per the
// order.* contracts.
func makeOrderEnvelope(eventID, eventType, orderID, merchantID, region string, extra map[string]any, at time.Time) (eventbus.Envelope, error) {
	payload := map[string]any{"order_id": orderID}
	if merchantID != "" {
		payload["merchant_id"] = merchantID
	}
	for k, v := range extra {
		payload[k] = v
	}
	if eventID == "" {
		eventID = newToken("evt")
	}
	return eventbus.NewEnvelope(
		eventID, eventType, "trace_"+orderID,
		eventbus.Aggregate{Type: "order", ID: orderID, Region: region},
		1, payload, at,
	)
}
