package main

import (
	"context"
	"database/sql"

	"github.com/shop-platform/shop/libs/eventbus"
	match "github.com/shop-platform/shop/services/dispatch/match"
)

// consumer.go — the projection that FEEDS the batch matcher. Two contract stubs
// drive dispatch (02 §4.3): order.paid makes an order WAITING in its zone, and
// driver.location_updated makes a driver AVAILABLE in its zone. Consumption is
// EXACTLY-ONCE via the durable SQL inbox (libs/inbox): a redelivered event_id
// collides on the inbox unique key and is a no-op, so a Kafka redelivery never
// double-registers an order or a driver. The engine mutation runs only on the
// first (applied) delivery.

// Projection consumes order.paid + driver.location_updated into the engine.
type Projection struct {
	eng   *match.Engine
	st    *store
	clock Clock
}

func newProjection(eng *match.Engine, st *store, clock Clock) *Projection {
	return &Projection{eng: eng, st: st, clock: clock}
}

// Handle is the eventbus.Handler. The inbox gives exactly-once effect; the engine
// mutation (AddOrder / AddDriver) runs only when the inbox actually applied the
// event (first delivery), so redelivery is a no-op. A foreign topic is a benign
// dedupe-committed no-op.
func (pr *Projection) Handle(ctx context.Context, msg eventbus.Message) error {
	env := msg.Envelope
	switch env.EventType {
	case TopicOrderPaid, TopicDriverLocation:
	default:
		return nil // not ours (forward compat)
	}
	applied, err := pr.st.inbx.Process(ctx, msg, func(ctx context.Context, tx *sql.Tx) error {
		// The inbox row itself is the exactly-once marker; no extra table write is
		// required for the engine effect (it runs post-commit on first apply).
		_ = tx
		return nil
	})
	if err != nil {
		return err
	}
	if !applied {
		return nil // redelivery — engine already has this order/driver
	}
	return pr.apply(env)
}

// apply mutates the engine from a first-delivery event.
func (pr *Projection) apply(env eventbus.Envelope) error {
	switch env.EventType {
	case TopicOrderPaid:
		p, err := decodePayload[orderPaidPayload](env.Payload)
		if err != nil {
			return err
		}
		orderID := p.OrderID
		if orderID == "" {
			orderID = env.Aggregate.ID
		}
		pickup := derivePickup(orDefault(p.MerchantID, orderID))
		if p.Pickup != nil {
			pickup = *p.Pickup
		}
		pr.eng.AddOrder(match.Order{OrderID: orderID, Pickup: pickup})
	case TopicDriverLocation:
		p, err := decodePayload[driverLocationPayload](env.Payload)
		if err != nil {
			return err
		}
		driverID := p.DriverID
		if driverID == "" {
			driverID = env.Aggregate.ID
		}
		pr.eng.AddDriver(match.Driver{DriverID: driverID, Loc: match.Point{Lat: p.Lat, Lng: p.Lng}})
	}
	return nil
}

// InjectEnvelope routes a raw envelope through the projection (the E2E stub-event
// delivery path — mirrors order's /v1/order-events — and test entry point).
// Returns the eventbus.Message it built so tests can redeliver the SAME event_id.
func (pr *Projection) InjectEnvelope(ctx context.Context, env eventbus.Envelope) (eventbus.Message, error) {
	if env.EventID == "" {
		env.EventID = newToken("evt")
	}
	msg, err := eventbus.NewMessage(env.EventType, env)
	if err != nil {
		return eventbus.Message{}, err
	}
	return msg, pr.Handle(ctx, msg)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
