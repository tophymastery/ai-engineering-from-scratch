package main

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"testing"
	"time"
)

// sign_test.go — the V-T8 headline security proof (property #2): an HMAC-signed
// quote whose body is TAMPERED, or whose (signed) expiry has PASSED, is rejected
// with 422 on 100% of a fixture sweep. Mirrors V-T1's 1000/1000 forgery rigor
// (services/identity-auth / libs/edgeauth). Plus the key-rotation rehearsal:
// an outgoing key still verifies in-flight quotes during the overlap window while
// the new key signs new quotes.

// signedBase mints a pristine, authentic signed quote at `now` (10-min TTL).
func signedBase(km *keyManager, now time.Time) *Quote {
	q := &Quote{
		QuoteID:  "qot_verify_base",
		CartID:   "crt_verify_base",
		Currency: "THB",
		Subtotal: money{40000, "THB"},
		Fees:     []lineItem{{"DELIVERY", 1900, "THB"}, {"SERVICE", 4000, "THB"}},
		Discounts: []lineItem{{"VOUCHER", -2500, "THB"}},
		Total:    money{43400, "THB"},
		IssuedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: now.Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
	if err := signQuote(km, q); err != nil {
		panic(err)
	}
	return q
}

func cloneQuote(q *Quote) *Quote {
	b, _ := json.Marshal(q)
	var c Quote
	_ = json.Unmarshal(b, &c)
	return &c
}

// TestTamperExpired_1000 is the property criterion: over N=1000 deterministic
// mutations (tampered amounts/line-items/cart-binding/expiry-extension, forged
// kid, corrupt signature, and authentic-but-expired), NOT A SINGLE quote may
// verify. Rejection rate must be 100% — every rejection maps to 422.
func TestTamperExpired_1000(t *testing.T) {
	clk := NewManualClock(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	km, err := newKeyManager(clk)
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	good := signedBase(km, now)

	// sanity: the pristine quote verifies.
	if err := verifyQuote(km, good, now); err != nil {
		t.Fatalf("pristine quote failed to verify: %v", err)
	}

	rng := mrand.New(mrand.NewSource(42))
	const N = 1000
	accepted := 0
	rejectedExpired := 0
	rejectedInvalid := 0
	for i := 0; i < N; i++ {
		m := cloneQuote(good)
		switch i % 8 {
		case 0: // flip a random byte of the signature
			bs := []byte(m.Signature)
			if len(bs) > 0 {
				pos := rng.Intn(len(bs))
				bs[pos] ^= byte(1 + rng.Intn(255))
				m.Signature = string(bs)
			}
		case 1: // tamper the total (attacker lowers what they pay)
			m.Total.Amount -= int64(1 + rng.Intn(40000))
		case 2: // tamper a fee line
			if len(m.Fees) > 0 {
				m.Fees[rng.Intn(len(m.Fees))].Amount += int64(1 + rng.Intn(5000))
			} else {
				m.Subtotal.Amount += 1
			}
		case 3: // extend the (signed) expiry to keep a stale quote alive
			m.ExpiresAt = now.Add(time.Duration(1+rng.Intn(1000)) * time.Hour).UTC().Format(time.RFC3339)
		case 4: // rebind the quote to a different cart
			m.CartID = fmt.Sprintf("crt_attacker_%d", i)
		case 5: // forged / unknown kid
			m.Kid = fmt.Sprintf("hk_forged_%d", i)
		case 6: // truncate the signature
			if len(m.Signature) > 2 {
				m.Signature = m.Signature[:1+rng.Intn(len(m.Signature)-1)]
			}
		case 7: // authentic signature but expired (re-sign with a past expiry)
			exp := now.Add(-time.Duration(1+rng.Intn(1000)) * time.Second)
			m.IssuedAt = exp.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			m.ExpiresAt = exp.UTC().Format(time.RFC3339)
			if err := signQuote(km, m); err != nil {
				t.Fatal(err)
			}
		}
		// Skip a no-op mutation (mutant identical to good).
		if m.Signature == good.Signature && m.Total == good.Total && m.CartID == good.CartID &&
			m.Kid == good.Kid && m.ExpiresAt == good.ExpiresAt && equalLines(m.Fees, good.Fees) {
			continue
		}
		err := verifyQuote(km, m, now)
		if err == nil {
			accepted++
			t.Errorf("mutation %d (kind %d) ACCEPTED a tampered/expired quote", i, i%8)
			continue
		}
		switch err {
		case errQuoteExpired:
			rejectedExpired++
		default:
			rejectedInvalid++
		}
	}
	if accepted != 0 {
		t.Fatalf("rejection rate %.2f%% — want 100%% (%d/%d accepted)",
			100*float64(N-accepted)/float64(N), accepted, N)
	}
	t.Logf("tamper/expiry rejection: %d/%d = 100%% (invalid=%d expired=%d)", N, N, rejectedInvalid, rejectedExpired)
}

// TestCheckoutTamper_HTTP_422 proves the 422 at the HTTP boundary AND that a
// tampered checkout persists ZERO PG rows (the tamper is caught before the write).
func TestCheckoutTamper_HTTP_422(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "LUNCH25", true))

	// Mutate the total downward, resubmit at checkout.
	q.Total.Amount = 1
	qb, _ := json.Marshal(q)
	code, m := do(t, h, "POST", "/v1/quotes/"+q.QuoteID+":checkout", string(qb))
	if code != 422 || errCode(m) != "QUOTE_INVALID" {
		t.Fatalf("tampered checkout -> %d %s (want 422 QUOTE_INVALID)", code, errCode(m))
	}
	// No PG row written for a rejected checkout.
	n, _ := s.st.quoteRowCount(context.Background())
	if n != 0 {
		t.Fatalf("tampered checkout wrote %d PG rows (want 0)", n)
	}
}

// TestCheckoutExpired_HTTP_422 advances the frozen clock past the quote's 10-min
// TTL and asserts checkout is rejected 422 QUOTE_EXPIRED, zero PG rows.
func TestCheckoutExpired_HTTP_422(t *testing.T) {
	s, clk, _ := newTestServer(t)
	h := s.mux()
	_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "", true))
	qb, _ := json.Marshal(q)
	clk.Advance(11 * time.Minute) // past the 10-min TTL
	code, m := do(t, h, "POST", "/v1/quotes/"+q.QuoteID+":checkout", string(qb))
	if code != 422 || errCode(m) != "QUOTE_EXPIRED" {
		t.Fatalf("expired checkout -> %d %s (want 422 QUOTE_EXPIRED)", code, errCode(m))
	}
	n, _ := s.st.quoteRowCount(context.Background())
	if n != 0 {
		t.Fatalf("expired checkout wrote %d PG rows (want 0)", n)
	}
}

// TestKeyRotationRunbook rehearses docs/runbooks/quote-key-rotation.md: sign a
// quote under key A → rotate to B (new quotes signed with B) → A-signed quote
// STILL verifies during the overlap → retire A → A-signed quote no longer
// verifies, B-signed still does.
func TestKeyRotationRunbook(t *testing.T) {
	clk := NewManualClock(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	km, err := newKeyManager(clk)
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	kidA := km.primaryKID()

	// Quote under key A.
	qA := signedBase(km, now)
	if qA.Kid != kidA {
		t.Fatalf("qA kid %q != primary A %q", qA.Kid, kidA)
	}

	// Rotate → key B is primary; ring holds A + B.
	kidB, err := km.rotate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if kidB == kidA {
		t.Fatal("rotate did not change the primary kid")
	}
	if len(km.kids()) != 2 {
		t.Fatalf("ring should hold 2 keys during overlap, got %v", km.kids())
	}

	// New quote signed with B.
	qB := signedBase(km, now)
	if qB.Kid != kidB {
		t.Fatalf("new quote not signed with B: kid=%q", qB.Kid)
	}

	// OVERLAP: both A and B verify.
	if err := verifyQuote(km, qA, now); err != nil {
		t.Fatalf("A-signed quote stopped verifying during overlap: %v", err)
	}
	if err := verifyQuote(km, qB, now); err != nil {
		t.Fatalf("B-signed quote failed: %v", err)
	}

	// Retire A. Now only B remains; A-signed quotes no longer verify.
	retired, err := km.retire()
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if retired != kidA {
		t.Fatalf("retired wrong kid: %q want %q", retired, kidA)
	}
	if err := verifyQuote(km, qA, now); err == nil {
		t.Fatal("A-signed quote still verified after A retired")
	}
	if err := verifyQuote(km, qB, now); err != nil {
		t.Fatalf("B-signed quote failed after retire: %v", err)
	}

	// retire refuses to drop the last/primary key.
	if _, err := km.retire(); err == nil {
		t.Fatal("retire should refuse to drop the only remaining key")
	}
}
