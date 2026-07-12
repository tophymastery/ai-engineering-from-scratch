package main

import (
	"encoding/json"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	match "github.com/shop-platform/shop/services/dispatch/match"
)

// events.go — the topic vocabulary + envelope helpers for the dispatch slice.
//
// CONSUMED (the "needs-dispatch" + location contract stubs, 02 §4.3):
//   - order.paid              → a paid order becomes a WAITING order in its zone.
//   - driver.location_updated → a driver's location makes it AVAILABLE in its zone.
//
// PRODUCED (D13 dispatch.offered/assigned/failed, key region:order_id):
//   - dispatch.offered  → a driver was reserved + offered this order (explainable).
//   - dispatch.assigned → the driver accepted; order → DISPATCHED (order consumes).
//   - dispatch.failed   → no driver within the batch; saga compensates (refund).
const (
	TopicOrderPaid       = "order.paid"
	TopicDriverLocation  = "driver.location_updated"
	TopicDispatchOffered = "dispatch.offered"
	TopicDispatchAssigned = "dispatch.assigned"
	TopicDispatchFailed  = "dispatch.failed"
)

// ConsumedTopics is the set of topics the dispatch projection subscribes to.
var ConsumedTopics = []string{TopicOrderPaid, TopicDriverLocation}

// orderPaidPayload mirrors contracts/events/order.paid/v1.schema.json (+ the
// additive-optional pickup the dispatch slice reads; when absent the pickup is
// derived deterministically from the merchant/order id so the demo is
// reproducible).
type orderPaidPayload struct {
	OrderID    string     `json:"order_id"`
	MerchantID string     `json:"merchant_id"`
	Pickup     *match.Point `json:"pickup"`
	PaidAt     string     `json:"paid_at"`
}

// driverLocationPayload mirrors contracts/events/driver.location_updated/v1.schema.json.
type driverLocationPayload struct {
	DriverID   string  `json:"driver_id"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	RecordedAt string  `json:"recorded_at"`
}

// derivePickup returns a deterministic pickup point for an order lacking explicit
// coordinates: a stable hash of the seed id into a Bangkok-area bounding box. Two
// events for the same order/merchant always yield the same pickup (so replay +
// re-dispatch are stable), and distinct merchants spread across zones.
func derivePickup(seed string) match.Point {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, b := range []byte(seed) {
		h ^= uint64(b)
		h *= prime
	}
	// Bangkok metro box ~ lat [13.60,13.95], lng [100.40,100.75].
	lat := 13.60 + float64(h%3500)/10000.0
	lng := 100.40 + float64((h>>20)%3500)/10000.0
	return match.Point{Lat: lat, Lng: lng}
}

// makeEnvelope builds a produced dispatch.* envelope about an order (key
// region:order_id, 02 §4.3).
func makeEnvelope(eventType, orderID, region string, payload map[string]any, at time.Time) (eventbus.Envelope, error) {
	return eventbus.NewEnvelope(
		newToken("evt"), eventType, "trace_"+orderID,
		eventbus.Aggregate{Type: "order", ID: orderID, Region: region},
		1, payload, at,
	)
}

// decodePayload is a small helper to unmarshal a typed payload from raw JSON.
func decodePayload[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}
