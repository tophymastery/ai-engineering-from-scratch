package main

import (
	"context"
	"database/sql"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
)

// saga.go orchestrates the order lifecycle (01 §4). It is the ONLY place a state
// change happens: every transition goes through transitionInTx, which
//   1. re-reads the order inside the tx,
//   2. asks the state machine (states.go) for (to, compensation) — illegal ⇒ 409,
//   3. applies a GUARDED UPDATE (WHERE status = from) so a concurrent transition
//      can never double-apply (the second sees 0 rows affected),
//   4. appends the order_events row (event store), arms/cancels the durable
//      timers the new state implies, and stages the order.* outbox event —
//      all atomically in one tx (01 §3 / D22),
// then runs the compensation side-effect (payment void/refund/capture) once,
// post-commit. Timeouts (T_accept/T_dispatch/remediation) drive the same path
// via the leased sweeper (timers.go).

var (
	codeOrderNotFound = shoperr.Register("ORDER_NOT_FOUND", 404, false, "No order exists with that id.")
	codeSagaDisabled  = shoperr.Register("SAGA_DISABLED", 404, false, "The saga_v1 feature is not enabled.")
)

// saga wires the store, the injected clock, and the payment adapter.
type saga struct {
	st     *store
	pay    PaymentClient
	region string
}

func newSaga(st *store, pay PaymentClient, region string) *saga {
	return &saga{st: st, pay: pay, region: region}
}

// transitionResult carries what a transition did (for post-commit compensation).
type transitionResult struct {
	Order   OrderRow
	From    State
	To      State
	Comp    Compensation
	Applied bool // false ⇒ lost race / already applied (idempotent no-op)
}

// transitionInTx performs one guarded state change on tx. It returns the
// compensation the caller must run post-commit. A 409 ORDER_INVALID_TRANSITION
// (or 404) is returned as a *shoperr.Error; a lost race (guarded UPDATE hit 0
// rows) returns Applied=false, err=nil.
func (sg *saga) transitionInTx(ctx context.Context, tx *sql.Tx, orderID string, trig Trigger, detail map[string]any, now time.Time) (transitionResult, error) {
	// Re-read inside the tx.
	var o OrderRow
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT order_id, customer_id, merchant_id, quote_id, region, status,
		        total_minor, currency, auth_id, capture_id, created_at, updated_at
		   FROM orders WHERE order_id = ?`, orderID).
		Scan(&o.OrderID, &o.CustomerID, &o.MerchantID, &o.QuoteID, &o.Region, &status,
			&o.Total.Amount, &o.Total.Currency, &o.AuthID, &o.CaptureID, &o.CreatedAt, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return transitionResult{}, shoperr.New(codeOrderNotFound, "")
	}
	if err != nil {
		return transitionResult{}, err
	}
	o.Status = State(status)

	to, comp, terr := Transition(o.Status, trig)
	if terr != nil {
		return transitionResult{Order: o, From: o.Status}, terr // ORDER_INVALID_TRANSITION (409)
	}

	// Guarded UPDATE: only transition if still in `from`. A racing transition
	// (event vs timer) makes exactly one of them see rowsAffected == 1.
	res, err := tx.ExecContext(ctx,
		`UPDATE orders SET status = ?, updated_at = ? WHERE order_id = ? AND status = ?`,
		string(to), now, orderID, string(o.Status))
	if err != nil {
		return transitionResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return transitionResult{Order: o, From: o.Status, Applied: false}, nil
	}

	if err := sg.st.appendEventSQLTx(ctx, tx, orderID, trig, o.Status, to, detail, now); err != nil {
		return transitionResult{}, err
	}
	if err := sg.armTimersSQLTx(ctx, tx, orderID, o.Status, to, now); err != nil {
		return transitionResult{}, err
	}
	if err := sg.stageDomainEventSQLTx(ctx, tx, o, to, now); err != nil {
		return transitionResult{}, err
	}
	o.Status = to
	return transitionResult{Order: o, From: State(status), To: to, Comp: comp, Applied: true}, nil
}

// ApplyTrigger runs one transition end-to-end (own tx + post-commit
// compensation). Used by the action endpoints (merchant accept/reject) and by
// the timer sweeper. Returns the updated order, whether it applied, and any
// 404/409 error.
func (sg *saga) ApplyTrigger(ctx context.Context, orderID string, trig Trigger, detail map[string]any, now time.Time) (OrderRow, bool, error) {
	tx, err := sg.st.db.BeginTx(ctx, nil)
	if err != nil {
		return OrderRow{}, false, err
	}
	r, err := sg.transitionInTx(ctx, tx, orderID, trig, detail, now)
	if err != nil {
		_ = tx.Rollback()
		return r.Order, false, err
	}
	if !r.Applied {
		_ = tx.Rollback()
		o, _, _ := sg.st.getOrder(ctx, orderID)
		return o, false, nil
	}
	if err := tx.Commit(); err != nil {
		return OrderRow{}, false, err
	}
	sg.st.ob.Signal()
	sg.runCompensation(ctx, r)
	o, _, _ := sg.st.getOrder(ctx, orderID)
	return o, true, nil
}

// runCompensation executes the transition's compensation exactly once,
// post-commit (01 §4). A crash between commit and here leaves the order in its
// new state with the compensation un-run; the durable timers are the safety net
// (e.g. a stuck PAYMENT_PENDING is voided+cancelled by the remediation timer).
func (sg *saga) runCompensation(ctx context.Context, r transitionResult) {
	o := r.Order
	switch r.Comp {
	case CompVoid:
		if o.AuthID != "" {
			_ = sg.pay.Void(ctx, o.AuthID)
		}
	case CompRefund:
		if o.CaptureID != "" {
			_ = sg.pay.Refund(ctx, o.CaptureID, o.Total)
		} else if o.AuthID != "" {
			_ = sg.pay.Void(ctx, o.AuthID) // pre-capture refund == void the hold
		}
	case CompCapture:
		if o.AuthID != "" {
			if capID, err := sg.pay.Capture(ctx, o.AuthID, o.Total); err == nil && capID != "" {
				_, _ = sg.st.db.ExecContext(ctx,
					`UPDATE orders SET capture_id = ? WHERE order_id = ?`, capID, o.OrderID)
			}
		}
	case CompRedispatch:
		// Re-dispatch is modelled by re-arming the T_dispatch timer (done in
		// armTimersSQLTx on the DISPATCHED→ACCEPTED transition) + the emitted
		// order.accepted event a dispatch consumer re-offers against. No payment
		// effect.
	}
}

// armTimersSQLTx arms/cancels the durable timers implied by entering `to`
// (01 §4 timeouts). All timer writes are in the transition tx, so a timer is
// durable the instant its state exists.
func (sg *saga) armTimersSQLTx(ctx context.Context, tx *sql.Tx, orderID string, from, to State, now time.Time) error {
	switch to {
	case StatePaid:
		// Left PAYMENT_PENDING ⇒ the remediation timer is moot; arm T_accept.
		if err := sg.st.cancelTimersSQLTx(ctx, tx, orderID, KindRemediation); err != nil {
			return err
		}
		_, err := sg.st.scheduleTimerSQLTx(ctx, tx, orderID, KindAccept, TrigAcceptTimeout, now.Add(DefaultAcceptWindow))
		return err
	case StateAccepted:
		// Reached ACCEPTED (fresh accept OR re-dispatch): cancel any T_accept,
		// (re-)arm T_dispatch.
		if err := sg.st.cancelTimersSQLTx(ctx, tx, orderID, KindAccept); err != nil {
			return err
		}
		if err := sg.st.cancelTimersSQLTx(ctx, tx, orderID, KindDispatch); err != nil {
			return err
		}
		_, err := sg.st.scheduleTimerSQLTx(ctx, tx, orderID, KindDispatch, TrigDispatchExhausted, now.Add(DefaultDispatchWindow))
		return err
	case StateDispatched:
		return sg.st.cancelTimersSQLTx(ctx, tx, orderID, KindDispatch)
	case StateDelivered:
		// Arm capture-by: capture the held auth by the horizon (auto-settle).
		_, err := sg.st.scheduleTimerSQLTx(ctx, tx, orderID, KindCaptureBy, TrigSettle, now.Add(DefaultCaptureByWindow))
		return err
	case StateCancelled, StateSettled:
		// Terminal: cancel every pending timer for the order.
		_, err := tx.ExecContext(ctx,
			`UPDATE timers SET status='CANCELLED' WHERE order_id=? AND status='PENDING'`, orderID)
		return err
	}
	return nil
}

// stageDomainEventSQLTx stages the order.* outbox event for entering `to`
// (02 §4.3). One event per state, keyed by the order aggregate (D5).
func (sg *saga) stageDomainEventSQLTx(ctx context.Context, tx *sql.Tx, o OrderRow, to State, now time.Time) error {
	topic, payload := domainEventFor(o, to, now)
	if topic == "" {
		return nil
	}
	env, err := buildEnvelope(topic, o, payload, now)
	if err != nil {
		return err
	}
	return sg.st.ob.WriteInTx(ctx, tx, topic, env)
}

// buildEnvelope constructs the 02 §4.3 envelope for an order event.
func buildEnvelope(topic string, o OrderRow, payload map[string]any, now time.Time) (eventbus.Envelope, error) {
	return eventbus.NewEnvelope(
		newToken("evt"), topic, "trace_"+o.OrderID,
		eventbus.Aggregate{Type: "order", ID: o.OrderID, Region: o.Region},
		1, payload, now,
	)
}

// domainEventFor maps a destination state to its published topic + payload.
func domainEventFor(o OrderRow, to State, now time.Time) (string, map[string]any) {
	ts := now.UTC().Format(time.RFC3339)
	total := map[string]any{"amount": o.Total.Amount, "currency": o.Total.Currency}
	switch to {
	case StatePaid:
		return "order.paid", map[string]any{"order_id": o.OrderID, "payment_id": o.AuthID, "total": total, "paid_at": ts}
	case StateAccepted:
		return "order.accepted", map[string]any{"order_id": o.OrderID, "merchant_id": o.MerchantID, "accepted_at": ts}
	case StateDispatched:
		return "order.dispatched", map[string]any{"order_id": o.OrderID, "dispatched_at": ts}
	case StatePickedUp:
		return "order.picked_up", map[string]any{"order_id": o.OrderID, "picked_up_at": ts}
	case StateDelivered:
		return "order.delivered", map[string]any{"order_id": o.OrderID, "delivered_at": ts}
	case StateSettled:
		return "order.settled", map[string]any{"order_id": o.OrderID, "capture_id": o.CaptureID, "settled_at": ts}
	case StateCancelled:
		return "order.cancelled", map[string]any{"order_id": o.OrderID, "cancelled_at": ts}
	}
	return "", nil
}
