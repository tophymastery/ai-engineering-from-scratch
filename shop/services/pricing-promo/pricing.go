package main

import (
	"time"
)

// pricing.go — the DETERMINISTIC quote engine (D10 / V-T8 correctness property
// #1). Given a cart snapshot (subtotal + currency, fetched from the cart
// contract), a delivery context, and an OPTIONAL promo/voucher code, it computes:
//
//	fees[]      = DELIVERY (flat base) + SERVICE (% of subtotal) + SURGE (only in
//	              a surge window, driven by the injected clock)
//	discounts[] = PROMO (% off subtotal) and/or VOUCHER (fixed off) — NEGATIVE
//	total       = subtotal + Σfees + Σdiscounts, clamped ≥ 0
//
// EVERYTHING is integer minor units (02 §1 Money) — never floats — so the math
// is exact and byte-identical on reruns. The only time input is the quote's
// issue time (surge window); freeze it and the whole computation is a pure
// function of (subtotal, currency, delivery, code, issuedAt). Unit-tested with
// frozen-clock fixtures in pricing_test.go.

// pricingConfig holds the tunable, deterministic rate table. Defaults are
// overridable via env (main.go) but are constants at quote time, so a given
// config + inputs always yields the same quote.
type pricingConfig struct {
	deliveryBaseMinor int64 // flat DELIVERY fee (minor units)
	serviceFeeBps     int64 // SERVICE fee as basis points of subtotal (1000 = 10%)
	surgeBps          int64 // surge multiplier in bps applied to the delivery base (15000 = 1.5×)
}

func defaultPricingConfig() pricingConfig {
	return pricingConfig{
		deliveryBaseMinor: 1900,  // 19.00 in the cart currency
		serviceFeeBps:     1000,  // 10% service fee
		surgeBps:          15000, // 1.5× during surge windows
	}
}

// promoRule is one deterministic promotion in the registry. kindPercent applies
// `value` basis points off the subtotal; kindFixed applies `value` minor units
// off. lineType is the typed discount line it produces (PROMO or VOUCHER).
type promoRule struct {
	lineType string
	percent  bool
	value    int64
}

// promoRegistry is the static, deterministic promo/voucher table. A code not in
// the table applies no discount (an invalid code silently doesn't apply — the
// quote still prices). Keeping it a fixed table (not a DB read) keeps the math
// deterministic and unit-testable; a real deployment would back this with the
// campaign service, but the ENGINE contract (typed discount lines, integer math)
// is identical.
var promoRegistry = map[string]promoRule{
	"LUNCH25":   {lineType: "VOUCHER", percent: false, value: 2500}, // 25.00 off (matches the contract example)
	"SAVE50":    {lineType: "VOUCHER", percent: false, value: 5000}, // 50.00 off
	"PROMO10":   {lineType: "PROMO", percent: true, value: 1000},    // 10% off subtotal
	"WELCOME20": {lineType: "PROMO", percent: true, value: 2000},    // 20% off subtotal
}

// quoteInputs is the resolved, pure input to the engine.
type quoteInputs struct {
	cartID    string
	subtotal  int64
	currency  string
	hasDelivery bool   // a delivery location was supplied (delivery fee applies)
	code      string   // promo/voucher code (may be empty / unknown)
	issuedAt  time.Time
}

// isSurgeWindow reports whether t (UTC) falls in a surge window. Deterministic:
// lunch 11:00–13:59 and dinner 18:00–20:59 local-UTC hours. Frozen-clock tests
// pin t to prove both surge and off-peak pricing.
func isSurgeWindow(t time.Time) bool {
	h := t.UTC().Hour()
	return (h >= 11 && h < 14) || (h >= 18 && h < 21)
}

// computeQuote is the pure pricing function. It returns the typed fees[],
// discounts[], and the clamped total. Given identical inputs + config it returns
// byte-identical output every time (no map iteration, no wall-clock, no floats;
// fees/discounts are appended in a FIXED order).
func computeQuote(cfg pricingConfig, in quoteInputs) (fees []lineItem, discounts []lineItem, total money) {
	cur := in.currency
	fees = []lineItem{}
	discounts = []lineItem{}

	// --- fees (positive), fixed order: DELIVERY, SERVICE, SURGE ---
	if in.hasDelivery {
		fees = append(fees, lineItem{Type: "DELIVERY", Amount: cfg.deliveryBaseMinor, Currency: cur})
	}
	if cfg.serviceFeeBps > 0 && in.subtotal > 0 {
		svc := in.subtotal * cfg.serviceFeeBps / 10000 // integer floor, exact
		if svc > 0 {
			fees = append(fees, lineItem{Type: "SERVICE", Amount: svc, Currency: cur})
		}
	}
	if in.hasDelivery && isSurgeWindow(in.issuedAt) && cfg.surgeBps > 10000 {
		// Surge is the delta above 1× applied to the delivery base.
		surge := cfg.deliveryBaseMinor * (cfg.surgeBps - 10000) / 10000
		if surge > 0 {
			fees = append(fees, lineItem{Type: "SURGE", Amount: surge, Currency: cur})
		}
	}

	// --- discounts (negative) from the promo/voucher code ---
	if rule, ok := promoRegistry[in.code]; ok && in.subtotal > 0 {
		var amt int64
		if rule.percent {
			amt = in.subtotal * rule.value / 10000 // integer floor
		} else {
			amt = rule.value
			if amt > in.subtotal {
				amt = in.subtotal // a fixed voucher never exceeds the subtotal
			}
		}
		if amt > 0 {
			discounts = append(discounts, lineItem{Type: rule.lineType, Amount: -amt, Currency: cur})
		}
	}

	// --- total = subtotal + Σfees + Σdiscounts, clamped ≥ 0 ---
	sum := in.subtotal
	for _, f := range fees {
		sum += f.Amount
	}
	for _, d := range discounts {
		sum += d.Amount // negative
	}
	if sum < 0 {
		sum = 0
	}
	return fees, discounts, money{Amount: sum, Currency: cur}
}
