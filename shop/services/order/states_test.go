package main

import (
	"testing"
)

// states_test.go proves the ENTIRE 01 §4 state machine: every legal transition
// (with its exact destination + compensation), and every illegal transition
// (⇒ 409 ORDER_INVALID_TRANSITION). It enumerates AllStates × AllTriggers so no
// combination is untested — the coverage counts are printed.

// goldenTransitions is the authoritative expectation, copied by hand from
// docs/01-architecture.md §4 (NOT from transitions[], so a table edit that
// diverges from the doc fails this test).
var goldenTransitions = []struct {
	from State
	trig Trigger
	to   State
	comp Compensation
}{
	{StateCreated, TrigQuote, StateQuoted, CompNone},
	{StateQuoted, TrigCheckout, StatePaymentPending, CompNone},
	{StatePaymentPending, TrigPaymentAuthorized, StatePaid, CompNone},
	{StatePaymentPending, TrigPaymentFailed, StateCancelled, CompNone},
	{StatePaymentPending, TrigUserCancel, StateCancelled, CompVoid},
	{StatePaymentPending, TrigPaymentTimeout, StateCancelled, CompVoid},
	{StatePaid, TrigMerchantAccept, StateAccepted, CompNone},
	{StatePaid, TrigMerchantReject, StateCancelled, CompRefund},
	{StatePaid, TrigAcceptTimeout, StateCancelled, CompRefund},
	{StateAccepted, TrigDispatchAssigned, StateDispatched, CompNone},
	{StateAccepted, TrigDispatchExhausted, StateCancelled, CompRefund},
	{StateDispatched, TrigPickup, StatePickedUp, CompNone},
	{StateDispatched, TrigDriverAbandon, StateAccepted, CompRedispatch},
	{StatePickedUp, TrigDelivered, StateDelivered, CompNone},
	{StateDelivered, TrigSettle, StateSettled, CompCapture},
}

// TestEveryLegalTransition: each of the 15 documented transitions produces the
// exact destination + compensation, with no error.
func TestEveryLegalTransition(t *testing.T) {
	if len(goldenTransitions) != len(transitions) {
		t.Fatalf("table has %d transitions, golden has %d — they must match the doc", len(transitions), len(goldenTransitions))
	}
	for _, g := range goldenTransitions {
		to, comp, err := Transition(g.from, g.trig)
		if err != nil {
			t.Fatalf("legal %s --(%s)--> unexpectedly errored: %v", g.from, g.trig, err)
		}
		if to != g.to {
			t.Fatalf("%s --(%s)--> got %s want %s", g.from, g.trig, to, g.to)
		}
		if comp != g.comp {
			t.Fatalf("%s --(%s)--> comp got %q want %q", g.from, g.trig, comp, g.comp)
		}
	}
	t.Logf("LEGAL transitions verified: %d/%d", len(goldenTransitions), len(transitions))
}

// TestEveryIllegalTransition: EVERY (state,trigger) pair NOT in the table is
// rejected with 409 ORDER_INVALID_TRANSITION. This is the "anything not listed
// is rejected" guarantee (01 §4/§6).
func TestEveryIllegalTransition(t *testing.T) {
	legal := map[[2]string]bool{}
	for _, g := range goldenTransitions {
		legal[[2]string{string(g.from), string(g.trig)}] = true
	}
	total, illegal := 0, 0
	for _, from := range AllStates {
		for _, trig := range AllTriggers {
			total++
			if legal[[2]string{string(from), string(trig)}] {
				continue
			}
			illegal++
			to, _, err := Transition(from, trig)
			if err == nil {
				t.Fatalf("illegal %s --(%s)--> %s did NOT error", from, trig, to)
			}
			if !isInvalidTransition(err) {
				t.Fatalf("illegal %s --(%s)--> wrong error: %v (want ORDER_INVALID_TRANSITION)", from, trig, err)
			}
			// The state must be unchanged (engine has no hidden state).
			if to != from {
				t.Fatalf("illegal transition mutated state: %s -> %s", from, to)
			}
		}
	}
	if legalCount := total - illegal; legalCount != len(goldenTransitions) {
		t.Fatalf("legal count via enumeration = %d, want %d", legalCount, len(goldenTransitions))
	}
	t.Logf("ILLEGAL transitions rejected 409: %d/%d combos (states=%d × triggers=%d = %d; legal=%d)",
		illegal, total, len(AllStates), len(AllTriggers), total, len(goldenTransitions))
}

// TestTerminalStates: SETTLED and CANCELLED have NO outgoing transition.
func TestTerminalStates(t *testing.T) {
	for _, s := range []State{StateSettled, StateCancelled} {
		if !IsTerminal(s) {
			t.Fatalf("%s should be terminal", s)
		}
		for _, trig := range AllTriggers {
			if _, _, err := Transition(s, trig); err == nil {
				t.Fatalf("terminal %s accepted trigger %s", s, trig)
			}
		}
	}
	// Non-terminal states must NOT be flagged terminal.
	for _, s := range []State{StateCreated, StateQuoted, StatePaymentPending, StatePaid, StateAccepted, StateDispatched, StatePickedUp, StateDelivered} {
		if IsTerminal(s) {
			t.Fatalf("%s should NOT be terminal", s)
		}
	}
}

// TestReachability: from CREATED every non-terminal state is reachable and both
// terminals (SETTLED, CANCELLED) are reachable — the machine has no dead states.
func TestReachability(t *testing.T) {
	adj := map[State][]State{}
	for _, tr := range transitions {
		adj[tr.From] = append(adj[tr.From], tr.To)
	}
	seen := map[State]bool{}
	var dfs func(State)
	dfs = func(s State) {
		if seen[s] {
			return
		}
		seen[s] = true
		for _, n := range adj[s] {
			dfs(n)
		}
	}
	dfs(StateCreated)
	for _, s := range AllStates {
		if !seen[s] {
			t.Fatalf("state %s is unreachable from CREATED", s)
		}
	}
}
