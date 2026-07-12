package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
)

// events.go is the payment slice's event surface (02 §4.3):
//
//   - PRODUCER: it builds the payment.* envelopes the money mutations stage into
//     the outbox — payment.authorized / payment.captured / payment.refunded /
//     payment.failed — keyed region:order_id (aggregate.type "order"), which the
//     order saga + settlement + notification consume.
//   - CONSUMER: it consumes ORDER contract stubs — order.delivered ⇒ auto-capture
//     the held authorization, order.cancelled ⇒ void it — EXACTLY-ONCE via the
//     durable SQL inbox (a redelivered order event is a no-op). This is the
//     event-level order↔payment integration ("contract, not code, is the surface").

// EmittedTopics is the set of payment.* topics this slice produces.
var EmittedTopics = []string{
	"payment.authorized", "payment.captured", "payment.refunded", "payment.failed",
}

// ConsumedOrderTopics is the set of order.* topics the payment service consumes.
var ConsumedOrderTopics = []string{"order.delivered", "order.cancelled"}

// buildEnvelope constructs the 02 §4.3 envelope for a payment event. Payment
// events key on the ORDER aggregate (region:order_id) per the published
// payment.authorized contract, so downstream order/settlement consumers partition
// them alongside the order's own events.
func buildEnvelope(topic string, p PaymentRow, payload map[string]any, now time.Time) (eventbus.Envelope, error) {
	return eventbus.NewEnvelope(
		newToken("evt"), topic, "trace_"+p.OrderID,
		eventbus.Aggregate{Type: "order", ID: p.OrderID, Region: p.Region},
		1, payload, now,
	)
}

// payment.* payload builders (each matches contracts/events/<topic>/v1.schema.json).

func authorizedPayload(p PaymentRow) map[string]any {
	return map[string]any{
		"payment_id": p.PaymentID, "order_id": p.OrderID,
		"amount": amountMap(p.Amount), "psp": p.PSP,
	}
}

func capturedPayload(p PaymentRow, now time.Time) map[string]any {
	return map[string]any{
		"payment_id": p.PaymentID, "order_id": p.OrderID, "capture_id": p.CaptureID,
		"amount": amountMap(p.Amount), "captured_at": now.UTC().Format(time.RFC3339),
	}
}

func refundedPayload(p PaymentRow, now time.Time) map[string]any {
	return map[string]any{
		"payment_id": p.PaymentID, "order_id": p.OrderID, "refund_id": p.RefundID,
		"amount": amountMap(p.Amount), "refunded_at": now.UTC().Format(time.RFC3339),
	}
}

func failedPayload(p PaymentRow, reason string, now time.Time) map[string]any {
	return map[string]any{
		"payment_id": p.PaymentID, "order_id": p.OrderID, "reason": reason,
		"failed_at": now.UTC().Format(time.RFC3339),
	}
}

func amountMap(m money) map[string]any {
	return map[string]any{"amount": m.Amount, "currency": m.Currency}
}

// --- order-event consumer (consumes order contract stubs) -------------------

// orderConsumer applies inbound order.* events to the matching payment with
// exactly-once effect (durable inbox). order.delivered ⇒ capture; order.cancelled
// ⇒ void. Both drive the SAME money-mutation core the API endpoints use, so the
// D9/exactly-once guarantees hold regardless of the trigger source.
type orderConsumer struct {
	pm    *payments
	st    *store
	clock Clock
}

func newOrderConsumer(pm *payments, st *store, clock Clock) *orderConsumer {
	return &orderConsumer{pm: pm, st: st, clock: clock}
}

// Handle is the eventbus.Handler for order.* events. The inbox gives exactly-once
// effect: a redelivered order event (same event_id) is a no-op. An illegal
// transition for a (stale) event is swallowed so the inbox row still commits.
func (c *orderConsumer) Handle(ctx context.Context, msg eventbus.Message) error {
	et := msg.Envelope.EventType
	if et != "order.delivered" && et != "order.cancelled" {
		return nil // not ours (forward compat)
	}
	orderID := msg.Envelope.Aggregate.ID
	now := c.clock.Now()

	_, err := c.st.inbx.Process(ctx, msg, func(ctx context.Context, tx *sql.Tx) error {
		p, ok, e := c.pm.lockPaymentByOrderTx(ctx, tx, orderID)
		if e != nil {
			return e
		}
		if !ok {
			return nil // no payment for this order yet — nothing to do, commit the dedupe row
		}
		switch et {
		case "order.delivered":
			_, e = c.pm.captureInTx(ctx, tx, p, "order.delivered", now)
		case "order.cancelled":
			_, e = c.pm.voidInTx(ctx, tx, p, "order.cancelled", now)
		}
		if e != nil && isInvalidTransition(e) {
			return nil // stale/illegal (already captured/refunded/voided) — dedupe-commit as a no-op
		}
		return e
	})
	return err
}

// InjectEnvelope routes a raw order envelope through the consumer (the E2E
// stub-event delivery path + the test redelivery entry point). Returns the
// eventbus.Message built so tests can redeliver the SAME event_id.
func (c *orderConsumer) InjectEnvelope(ctx context.Context, env eventbus.Envelope) (eventbus.Message, error) {
	msg, err := eventbus.NewMessage(env.EventType, env)
	if err != nil {
		return eventbus.Message{}, err
	}
	return msg, c.Handle(ctx, msg)
}

// makeOrderEnvelope builds an inbound order-event envelope (injection endpoint + tests).
func makeOrderEnvelope(eventID, eventType, orderID, region string, payload map[string]any, at time.Time) (eventbus.Envelope, error) {
	if payload == nil {
		payload = map[string]any{"order_id": orderID}
	}
	return eventbus.NewEnvelope(
		eventID, eventType, "trace_"+orderID,
		eventbus.Aggregate{Type: "order", ID: orderID, Region: region},
		1, payload, at,
	)
}

// isInvalidTransition reports whether err is the 409 PAYMENT_INVALID_TRANSITION.
func isInvalidTransition(err error) bool {
	var pe *shoperr.Error
	if errors.As(err, &pe) {
		return pe.Code == "PAYMENT_INVALID_TRANSITION"
	}
	return false
}
