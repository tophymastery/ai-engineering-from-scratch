package main

// states.go is the AUTHORITATIVE order state machine (docs/01-architecture.md §4
// "Order state machine"). The transition table below is DATA — the engine
// rejects anything not listed with 409 ORDER_INVALID_TRANSITION (01 §6: "state
// machine over ad-hoc ifs"). Every legal transition, every illegal transition,
// and the compensation each transition triggers are unit-tested exhaustively in
// states_test.go. current state is a pure fold over order_events (store.go), so
// any order can be replayed for audit/debugging (01 §6 event-sourced history).

import (
	shoperr "github.com/shop-platform/shop/libs/errors"
)

// codeInvalidTransition is the 409 this slice returns for any transition not in
// the table. The name matches the shipped contract example
// (contracts/openapi/order.v1.yaml → ORDER_INVALID_TRANSITION).
var codeInvalidTransition = shoperr.Register("ORDER_INVALID_TRANSITION", 409, false,
	"The order is not in a state that allows this transition.")

// State is one order lifecycle state (01 §4). UPPER_SNAKE closed set.
type State string

const (
	StateCreated        State = "CREATED"
	StateQuoted         State = "QUOTED"
	StatePaymentPending State = "PAYMENT_PENDING"
	StatePaid           State = "PAID"
	StateAccepted       State = "ACCEPTED"
	StateDispatched     State = "DISPATCHED"
	StatePickedUp       State = "PICKED_UP"
	StateDelivered      State = "DELIVERED"
	StateSettled        State = "SETTLED"
	StateCancelled      State = "CANCELLED"
)

// AllStates is the closed set (for exhaustive testing + validation).
var AllStates = []State{
	StateCreated, StateQuoted, StatePaymentPending, StatePaid, StateAccepted,
	StateDispatched, StatePickedUp, StateDelivered, StateSettled, StateCancelled,
}

// Trigger is a transition trigger (a saga command result, a domain event, a
// timer firing, or an operator action). It is distinct from the Kafka domain
// event names — several triggers may map to one emitted event.
type Trigger string

const (
	TrigQuote             Trigger = "quote_ok"           // CREATED        -> QUOTED
	TrigCheckout          Trigger = "checkout_confirmed" // QUOTED         -> PAYMENT_PENDING
	TrigPaymentAuthorized Trigger = "payment_authorized" // PAYMENT_PENDING -> PAID
	TrigPaymentFailed     Trigger = "payment_failed"     // PAYMENT_PENDING -> CANCELLED
	TrigUserCancel        Trigger = "user_cancel"        // PAYMENT_PENDING -> CANCELLED (void)
	TrigPaymentTimeout    Trigger = "payment_timeout"    // PAYMENT_PENDING -> CANCELLED (remediation: void)
	TrigMerchantAccept    Trigger = "merchant_accept"    // PAID           -> ACCEPTED
	TrigMerchantReject    Trigger = "merchant_reject"    // PAID           -> CANCELLED [refund]
	TrigAcceptTimeout     Trigger = "accept_timeout"     // PAID           -> CANCELLED [refund]
	TrigDispatchAssigned  Trigger = "dispatch_assigned"  // ACCEPTED       -> DISPATCHED
	TrigDispatchExhausted Trigger = "dispatch_exhausted" // ACCEPTED       -> CANCELLED [refund]
	TrigPickup            Trigger = "driver_pickup"      // DISPATCHED     -> PICKED_UP
	TrigDriverAbandon     Trigger = "driver_abandon"     // DISPATCHED     -> ACCEPTED (re-dispatch)
	TrigDelivered         Trigger = "driver_delivered"   // PICKED_UP      -> DELIVERED
	TrigSettle            Trigger = "capture_settle"     // DELIVERED      -> SETTLED (capture + accrual)
)

// AllTriggers is the closed set (for exhaustive testing).
var AllTriggers = []Trigger{
	TrigQuote, TrigCheckout, TrigPaymentAuthorized, TrigPaymentFailed, TrigUserCancel,
	TrigPaymentTimeout, TrigMerchantAccept, TrigMerchantReject, TrigAcceptTimeout,
	TrigDispatchAssigned, TrigDispatchExhausted, TrigPickup, TrigDriverAbandon,
	TrigDelivered, TrigSettle,
}

// Compensation is the side effect the saga must run WHEN a transition fires
// (01 §4 "On failure (compensation)"). It is a property of the transition, not
// of the state, so the saga stays declarative.
type Compensation string

const (
	CompNone       Compensation = ""           // forward step, no compensation
	CompVoid       Compensation = "VOID"       // void a pending/authorized (uncaptured) payment
	CompRefund     Compensation = "REFUND"     // refund: void the auth (pre-capture) or refund the capture
	CompRedispatch Compensation = "REDISPATCH" // driver abandoned -> re-offer to another driver
	CompCapture    Compensation = "CAPTURE"    // delivered -> capture the held auth + settlement accrual
)

// transition is one row of the 01 §4 table.
type transition struct {
	From    State
	Trigger Trigger
	To      State
	Comp    Compensation
}

// transitions is the ENTIRE legal transition table (01 §4). Anything not here is
// rejected. Keep this list byte-for-byte aligned with the doc.
var transitions = []transition{
	{StateCreated, TrigQuote, StateQuoted, CompNone},
	{StateQuoted, TrigCheckout, StatePaymentPending, CompNone},
	{StatePaymentPending, TrigPaymentAuthorized, StatePaid, CompNone},
	{StatePaymentPending, TrigPaymentFailed, StateCancelled, CompNone},   // auth failed: nothing to void
	{StatePaymentPending, TrigUserCancel, StateCancelled, CompVoid},      // void any pending hold
	{StatePaymentPending, TrigPaymentTimeout, StateCancelled, CompVoid},  // remediation: >15min ⇒ void + cancel
	{StatePaid, TrigMerchantAccept, StateAccepted, CompNone},
	{StatePaid, TrigMerchantReject, StateCancelled, CompRefund},          // [refund]
	{StatePaid, TrigAcceptTimeout, StateCancelled, CompRefund},           // T_accept timeout [refund]
	{StateAccepted, TrigDispatchAssigned, StateDispatched, CompNone},
	{StateAccepted, TrigDispatchExhausted, StateCancelled, CompRefund},   // T_dispatch exhausted [refund]
	{StateDispatched, TrigPickup, StatePickedUp, CompNone},
	{StateDispatched, TrigDriverAbandon, StateAccepted, CompRedispatch},  // abandon -> re-dispatch
	{StatePickedUp, TrigDelivered, StateDelivered, CompNone},
	{StateDelivered, TrigSettle, StateSettled, CompCapture},              // capture + settlement accrual
}

// transitionIndex is the (from,trigger) → transition lookup built once at init.
var transitionIndex = func() map[[2]string]transition {
	m := make(map[[2]string]transition, len(transitions))
	for _, t := range transitions {
		m[[2]string{string(t.From), string(t.Trigger)}] = t
	}
	return m
}()

// IsTerminal reports whether a state has no outgoing transitions (SETTLED /
// CANCELLED). A trigger from a terminal state is always ORDER_INVALID_TRANSITION.
func IsTerminal(s State) bool { return s == StateSettled || s == StateCancelled }

// Transition applies tr to from, returning the destination state and the
// compensation the saga must run, or a 409 ORDER_INVALID_TRANSITION error if the
// (from,trigger) pair is not in the table. This is the ONLY place a state change
// is decided — the engine has no hidden state.
func Transition(from State, tr Trigger) (State, Compensation, error) {
	t, ok := transitionIndex[[2]string{string(from), string(tr)}]
	if !ok {
		return from, CompNone, shoperr.New(codeInvalidTransition, "",
			shoperr.Detail{Field: "status", Reason: "no transition " + string(from) + " --(" + string(tr) + ")-->"})
	}
	return t.To, t.Comp, nil
}

// CanTransition reports whether (from,trigger) is legal without applying it.
func CanTransition(from State, tr Trigger) bool {
	_, ok := transitionIndex[[2]string{string(from), string(tr)}]
	return ok
}
