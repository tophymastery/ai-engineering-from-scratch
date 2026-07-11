package main

import (
	"encoding/json"
	"testing"
	"time"
)

// pricing_test.go — DETERMINISTIC pricing-math proof (V-T8 property #1). The
// engine is a pure function of (subtotal, currency, delivery, code, issuedAt) +
// config: integer minor units only, surge from a frozen clock. These tests pin
// the clock and assert exact typed fees[]/discounts[]/total, and that reruns are
// byte-identical.

var offPeak = time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)  // 09:00 — no surge
var lunch = time.Date(2026, 7, 11, 12, 30, 0, 0, time.UTC)  // 12:30 — surge window
var dinner = time.Date(2026, 7, 11, 19, 15, 0, 0, time.UTC) // 19:15 — surge window

// TestPricingFixtures is a table of frozen-input fixtures with EXACT expected
// output. Integer math throughout; no floats; every number checked.
func TestPricingFixtures(t *testing.T) {
	cfg := defaultPricingConfig()
	type fix struct {
		name       string
		subtotal   int64
		delivery   bool
		code       string
		at         time.Time
		wantFees   []lineItem
		wantDisc   []lineItem
		wantTotal  int64
	}
	thb := "THB"
	fixtures := []fix{
		{
			name: "offpeak+delivery+voucher", subtotal: 40000, delivery: true, code: "LUNCH25", at: offPeak,
			wantFees:  []lineItem{{"DELIVERY", 1900, thb}, {"SERVICE", 4000, thb}},
			wantDisc:  []lineItem{{"VOUCHER", -2500, thb}},
			wantTotal: 43400, // 40000 + 1900 + 4000 - 2500
		},
		{
			name: "lunch surge adds SURGE line", subtotal: 40000, delivery: true, code: "", at: lunch,
			// SURGE = 1900 * (15000-10000)/10000 = 950.
			wantFees:  []lineItem{{"DELIVERY", 1900, thb}, {"SERVICE", 4000, thb}, {"SURGE", 950, thb}},
			wantDisc:  []lineItem{},
			wantTotal: 46850, // 40000 + 1900 + 4000 + 950
		},
		{
			name: "dinner surge + percent promo", subtotal: 30000, delivery: true, code: "PROMO10", at: dinner,
			// SERVICE 10% of 30000 = 3000; SURGE 950; PROMO 10% of 30000 = -3000.
			wantFees:  []lineItem{{"DELIVERY", 1900, thb}, {"SERVICE", 3000, thb}, {"SURGE", 950, thb}},
			wantDisc:  []lineItem{{"PROMO", -3000, thb}},
			wantTotal: 32850, // 30000 + 1900 + 3000 + 950 - 3000
		},
		{
			name: "no delivery ⇒ no DELIVERY/SURGE, service only", subtotal: 20000, delivery: false, code: "", at: lunch,
			wantFees:  []lineItem{{"SERVICE", 2000, thb}},
			wantDisc:  []lineItem{},
			wantTotal: 22000,
		},
		{
			name: "voucher capped at subtotal, service floors", subtotal: 3333, delivery: false, code: "SAVE50", at: offPeak,
			// SERVICE 10% of 3333 = 333 (floor); SAVE50 fixed 5000 capped to 3333.
			wantFees:  []lineItem{{"SERVICE", 333, thb}},
			wantDisc:  []lineItem{{"VOUCHER", -3333, thb}},
			wantTotal: 333, // 3333 + 333 - 3333
		},
		{
			name: "unknown code ⇒ no discount", subtotal: 10000, delivery: true, code: "NOPE", at: offPeak,
			wantFees:  []lineItem{{"DELIVERY", 1900, thb}, {"SERVICE", 1000, thb}},
			wantDisc:  []lineItem{},
			wantTotal: 12900,
		},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			fees, disc, total := computeQuote(cfg, quoteInputs{
				cartID: "crt_x", subtotal: f.subtotal, currency: thb,
				hasDelivery: f.delivery, code: f.code, issuedAt: f.at,
			})
			if !equalLines(fees, f.wantFees) {
				t.Fatalf("fees = %+v want %+v", fees, f.wantFees)
			}
			if !equalLines(disc, f.wantDisc) {
				t.Fatalf("discounts = %+v want %+v", disc, f.wantDisc)
			}
			if total.Amount != f.wantTotal || total.Currency != thb {
				t.Fatalf("total = %+v want %d %s", total, f.wantTotal, thb)
			}
		})
	}
}

// TestPricingDeterministic_ByteIdentical runs the same inputs 1000× and asserts
// the marshalled output is byte-identical every time (no map iteration, no
// wall-clock, no float drift).
func TestPricingDeterministic_ByteIdentical(t *testing.T) {
	cfg := defaultPricingConfig()
	in := quoteInputs{cartID: "crt_det", subtotal: 41234, currency: "THB", hasDelivery: true, code: "PROMO10", issuedAt: lunch}
	fees0, disc0, tot0 := computeQuote(cfg, in)
	b0, _ := json.Marshal(map[string]any{"fees": fees0, "discounts": disc0, "total": tot0})
	for i := 0; i < 1000; i++ {
		fees, disc, tot := computeQuote(cfg, in)
		b, _ := json.Marshal(map[string]any{"fees": fees, "discounts": disc, "total": tot})
		if string(b) != string(b0) {
			t.Fatalf("run %d not byte-identical:\n got %s\nwant %s", i, b, b0)
		}
	}
	t.Logf("byte-identical over 1000 reruns: %s", b0)
}

// TestSurgeWindowBoundaries pins the surge-window edges (deterministic).
func TestSurgeWindowBoundaries(t *testing.T) {
	cases := []struct {
		h    int
		want bool
	}{
		{10, false}, {11, true}, {13, true}, {14, false}, {17, false}, {18, true}, {20, true}, {21, false},
	}
	for _, c := range cases {
		got := isSurgeWindow(time.Date(2026, 7, 11, c.h, 0, 0, 0, time.UTC))
		if got != c.want {
			t.Fatalf("hour %d surge=%v want %v", c.h, got, c.want)
		}
	}
}

func equalLines(a, b []lineItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
