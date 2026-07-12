package main

// states.go is the AUTHORITATIVE payment state machine. Like the order slice
// (services/order/states.go) the transition table is DATA — the engine rejects
// any transition not listed with 409 PAYMENT_INVALID_TRANSITION. A payment is
// born on authorize (AUTHORIZED or DECLINED/FAILED); capture/refund/void are
// the money mutations that move it forward. Current state is a pure fold over
// the append-only payment_events store (store.go), so any payment can be
// replayed for audit — the money source of truth is the event log, not a
// mutable flag.

import (
	shoperr "github.com/shop-platform/shop/libs/errors"
)

// codeInvalidTransition is the 409 returned for any transition not in the table
// (e.g. capturing a refunded payment, refunding an uncaptured one).
var codeInvalidTransition = shoperr.Register("PAYMENT_INVALID_TRANSITION", 409, false,
	"The payment is not in a state that allows this money mutation.")

// State is one payment lifecycle state. UPPER_SNAKE closed set.
type State string

const (
	StateAuthorized State = "AUTHORIZED" // PSP hold placed (money reserved)
	StateCaptured   State = "CAPTURED"   // funds captured (settled)
	StateRefunded   State = "REFUNDED"   // captured funds reversed
	StateVoided     State = "VOIDED"     // uncaptured hold released
	StateDeclined   State = "DECLINED"   // authorize declined by the issuer (terminal)
	StateFailed     State = "FAILED"     // authorize errored/timed out (terminal)
)

// AllStates is the closed set (for exhaustive testing + validation).
var AllStates = []State{
	StateAuthorized, StateCaptured, StateRefunded, StateVoided, StateDeclined, StateFailed,
}

// Trigger is a money-mutation trigger (an API action, a consumed order event, a
// PSP webhook). Distinct from the emitted payment.* event names.
type Trigger string

const (
	TrigCapture Trigger = "capture" // AUTHORIZED -> CAPTURED
	TrigRefund  Trigger = "refund"  // CAPTURED   -> REFUNDED
	TrigVoid    Trigger = "void"    // AUTHORIZED -> VOIDED (release the hold)
)

// AllTriggers is the closed set (for exhaustive testing).
var AllTriggers = []Trigger{TrigCapture, TrigRefund, TrigVoid}

// transition is one row of the payment state table.
type transition struct {
	From    State
	Trigger Trigger
	To      State
}

// transitions is the ENTIRE legal transition table. Anything not here is a 409.
var transitions = []transition{
	{StateAuthorized, TrigCapture, StateCaptured},
	{StateAuthorized, TrigVoid, StateVoided},
	{StateCaptured, TrigRefund, StateRefunded},
}

// transitionIndex is the (from,trigger) → destination lookup built once at init.
var transitionIndex = func() map[[2]string]State {
	m := make(map[[2]string]State, len(transitions))
	for _, t := range transitions {
		m[[2]string{string(t.From), string(t.Trigger)}] = t.To
	}
	return m
}()

// IsTerminal reports whether a state has no outgoing transitions.
func IsTerminal(s State) bool {
	return s == StateRefunded || s == StateVoided || s == StateDeclined || s == StateFailed
}

// Transition applies tr to from, returning the destination state, or a 409
// PAYMENT_INVALID_TRANSITION if the (from,trigger) pair is not in the table.
// This is the ONLY place a payment status changes.
func Transition(from State, tr Trigger) (State, error) {
	to, ok := transitionIndex[[2]string{string(from), string(tr)}]
	if !ok {
		return from, shoperr.New(codeInvalidTransition, "",
			shoperr.Detail{Field: "status", Reason: "no transition " + string(from) + " --(" + string(tr) + ")-->"})
	}
	return to, nil
}

// CanTransition reports whether (from,trigger) is legal without applying it.
func CanTransition(from State, tr Trigger) bool {
	_, ok := transitionIndex[[2]string{string(from), string(tr)}]
	return ok
}
