package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
)

// events.go is the saga's CONSUMER side (01 §4 / 02 §4.3): payment.*, dispatch.*
// and driver.* events drive the order forward. Consumption is EXACTLY-ONCE via
// the durable SQL inbox (libs/inbox): a redelivered event_id collides on the
// inbox unique key and is a no-op, so a Kafka redelivery produces at most one
// state effect. The transition itself runs on the inbox's transaction, so the
// order_events row + the inbox dedupe row + the follow-on outbox event commit
// atomically (D22 exactly-once).

// eventTriggers maps a consumed domain event to the saga trigger it fires.
var eventTriggers = map[string]Trigger{
	"payment.authorized": TrigPaymentAuthorized,
	"payment.failed":     TrigPaymentFailed,
	"dispatch.assigned":  TrigDispatchAssigned,
	"dispatch.failed":    TrigDispatchExhausted,
	"driver.picked_up":   TrigPickup,
	"driver.delivered":   TrigDelivered,
	"driver.abandoned":   TrigDriverAbandon,
}

// ConsumedTopics is the set of topics the order saga subscribes to.
var ConsumedTopics = []string{
	"payment.authorized", "payment.failed",
	"dispatch.assigned", "dispatch.failed",
	"driver.picked_up", "driver.delivered", "driver.abandoned",
}

// sagaConsumer applies inbound domain events to the saga with exactly-once
// effect.
type sagaConsumer struct {
	sg    *saga
	st    *store
	clock Clock
}

func newSagaConsumer(sg *saga, st *store, clock Clock) *sagaConsumer {
	return &sagaConsumer{sg: sg, st: st, clock: clock}
}

// Handle is the eventbus.Handler. The inbox gives exactly-once effect; the
// transition runs on the inbox tx so it commits atomically with the dedupe row.
// An illegal transition for a (stale) event is swallowed — the order already
// moved on — so the inbox row still commits and the event is not re-parked.
func (c *sagaConsumer) Handle(ctx context.Context, msg eventbus.Message) error {
	trig, ok := eventTriggers[msg.Envelope.EventType]
	if !ok {
		return nil // not one of ours (forward compat)
	}
	orderID := msg.Envelope.Aggregate.ID
	now := c.clock.Now()

	var applied *transitionResult
	inserted, err := c.st.inbx.Process(ctx, msg, func(ctx context.Context, tx *sql.Tx) error {
		r, e := c.sg.transitionInTx(ctx, tx, orderID, trig, map[string]any{"event_id": msg.Envelope.EventID}, now)
		if e != nil {
			if isInvalidTransition(e) {
				return nil // stale/illegal — dedupe-commit as a no-op, don't retry
			}
			return e // real error (incl. NOT_FOUND) — roll back, retry/park
		}
		if r.Applied {
			rc := r
			applied = &rc
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Post-commit compensation runs only on the FIRST delivery that actually
	// applied a compensating transition (inserted=true ⇒ not a duplicate).
	if inserted && applied != nil {
		c.sg.st.ob.Signal()
		c.sg.runCompensation(ctx, *applied)
	}
	return nil
}

// InjectEnvelope routes a raw envelope through the consumer (the E2E stub-event
// delivery path — mirrors cart's /v1/menu-events — and the redelivery-fixture
// test entry point). Returns the eventbus.Message it built so tests can redeliver
// the SAME event_id.
func (c *sagaConsumer) InjectEnvelope(ctx context.Context, env eventbus.Envelope) (eventbus.Message, error) {
	msg, err := eventbus.NewMessage(env.EventType, env)
	if err != nil {
		return eventbus.Message{}, err
	}
	return msg, c.Handle(ctx, msg)
}

// isInvalidTransition reports whether err is the 409 ORDER_INVALID_TRANSITION.
func isInvalidTransition(err error) bool {
	var pe *shoperr.Error
	if errors.As(err, &pe) {
		return pe.Code == "ORDER_INVALID_TRANSITION"
	}
	return false
}

// makeDomainEnvelope builds an inbound domain-event envelope (used by the
// injection endpoint + tests) for a topic about an order.
func makeDomainEnvelope(eventID, eventType, orderID, region string, payload map[string]any, at time.Time) (eventbus.Envelope, error) {
	if payload == nil {
		payload = map[string]any{"order_id": orderID}
	}
	return eventbus.NewEnvelope(
		eventID, eventType, "trace_"+orderID,
		eventbus.Aggregate{Type: "order", ID: orderID, Region: region},
		1, payload, at,
	)
}
