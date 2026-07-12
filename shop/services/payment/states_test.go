package main

import "testing"

// states_test.go exhaustively exercises the payment state machine (states.go):
// every legal transition to its exact destination, every illegal transition to
// 409 PAYMENT_INVALID_TRANSITION (state unchanged), and the terminal set.

// TestEveryLegalTransition: all 3 legal transitions resolve to the right state.
func TestEveryLegalTransition(t *testing.T) {
	for _, tr := range transitions {
		to, err := Transition(tr.From, tr.Trigger)
		if err != nil {
			t.Fatalf("legal %s --(%s)--> unexpectedly errored: %v", tr.From, tr.Trigger, err)
		}
		if to != tr.To {
			t.Fatalf("%s --(%s)--> %s want %s", tr.From, tr.Trigger, to, tr.To)
		}
	}
	if len(transitions) != 3 {
		t.Fatalf("transition table has %d rows want 3 (AUTHORIZED→CAPTURED, AUTHORIZED→VOIDED, CAPTURED→REFUNDED)", len(transitions))
	}
	t.Logf("STATE MACHINE: %d/%d legal transitions verified to exact destination", len(transitions), len(transitions))
}

// TestEveryIllegalTransition: every (state, trigger) not in the table ⇒ 409, and
// Transition returns the unchanged from-state.
func TestEveryIllegalTransition(t *testing.T) {
	illegal := 0
	for _, s := range AllStates {
		for _, tr := range AllTriggers {
			if CanTransition(s, tr) {
				continue
			}
			to, err := Transition(s, tr)
			if err == nil {
				t.Fatalf("illegal %s --(%s)--> %s did not error", s, tr, to)
			}
			if !isInvalidTransition(err) {
				t.Fatalf("illegal %s --(%s)--> wrong error %v", s, tr, err)
			}
			if to != s {
				t.Fatalf("illegal transition changed state %s -> %s", s, to)
			}
			illegal++
		}
	}
	// 6 states × 3 triggers = 18 combos − 3 legal = 15 illegal.
	if illegal != 15 {
		t.Fatalf("illegal transition count %d want 15", illegal)
	}
	t.Logf("STATE MACHINE: %d/15 illegal transitions ⇒ 409 PAYMENT_INVALID_TRANSITION, state unchanged", illegal)
}

// TestTerminalStates: REFUNDED/VOIDED/DECLINED/FAILED are terminal (no out-edges);
// AUTHORIZED/CAPTURED are not.
func TestTerminalStates(t *testing.T) {
	for _, s := range []State{StateRefunded, StateVoided, StateDeclined, StateFailed} {
		if !IsTerminal(s) {
			t.Fatalf("%s should be terminal", s)
		}
		for _, tr := range AllTriggers {
			if CanTransition(s, tr) {
				t.Fatalf("terminal %s has an outgoing transition (%s)", s, tr)
			}
		}
	}
	for _, s := range []State{StateAuthorized, StateCaptured} {
		if IsTerminal(s) {
			t.Fatalf("%s should not be terminal", s)
		}
	}
}
