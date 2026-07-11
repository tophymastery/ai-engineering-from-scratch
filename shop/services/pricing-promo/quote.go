package main

import (
	"strconv"
	"strings"
)

// quote.go — the wire types of the V-T8 pricing slice (02 §1 Money + 02 §5 typed
// line-item lists) and the CANONICAL byte encoding the HMAC signs. Keeping the
// canonical encoding here (not JSON map order) is what makes the signature
// tamper-evident and reproducible: any economically-meaningful mutation changes
// these bytes, so the HMAC no longer matches (→ 422).

// money is the 02 §1 Money type: integer minor units + ISO currency; never
// floats. Deterministic pricing math (pricing.go) works entirely in this type.
type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// lineItem is one typed fee or discount (02 §5 "typed line-item lists instead of
// scalar fields — fees[], discounts[]"). Fees carry POSITIVE amounts; discounts
// carry NEGATIVE amounts (matching the contract example {type:"VOUCHER",
// amount:-2500}), so total = subtotal + Σfees + Σdiscounts is a single sum.
type lineItem struct {
	Type     string `json:"type"`   // fees: DELIVERY|SERVICE|SURGE ; discounts: PROMO|VOUCHER
	Amount   int64  `json:"amount"` // minor units; discounts negative
	Currency string `json:"currency"`
}

// Quote is the priced cart returned by POST /v1/quotes. It is HMAC-signed
// (Signature over the canonical bytes below, keyed by Kid) and lives in the
// Redis-like TTL tier for its 10-min life; PG persistence happens ONLY at
// checkout (D10). ExpiresAt is inside the signed payload, so a client cannot
// extend a quote's life without invalidating the signature.
type Quote struct {
	QuoteID   string     `json:"quote_id"`
	CartID    string     `json:"cart_id"`
	Currency  string     `json:"currency"`
	Subtotal  money      `json:"subtotal"`
	Fees      []lineItem `json:"fees"`
	Discounts []lineItem `json:"discounts"`
	Total     money      `json:"total"`
	IssuedAt  string     `json:"issued_at"`  // RFC 3339 UTC
	ExpiresAt string     `json:"expires_at"` // RFC 3339 UTC (issued + 10 min)
	Kid       string     `json:"kid"`        // signing-key id (rotation)
	Signature string     `json:"signature"`  // base64url HMAC-SHA256 over canonicalQuoteBytes
}

// canonicalQuoteBytes renders the signed portion of a quote in a FIXED field
// order (never JSON map order). It covers every economically-meaningful field —
// ids, currency, subtotal, every fee/discount line, total, issue+expiry — so any
// mutation an attacker makes to amounts, line items, expiry, or the bound cart
// changes these bytes and breaks the HMAC. Kid and Signature are excluded (the
// verifier resolves the key by Kid and compares against Signature).
func canonicalQuoteBytes(q *Quote) []byte {
	var b strings.Builder
	b.WriteString("pricing.quote.v1\n")
	b.WriteString(q.QuoteID)
	b.WriteByte('\n')
	b.WriteString(q.CartID)
	b.WriteByte('\n')
	b.WriteString(q.Currency)
	b.WriteByte('\n')
	writeMoney(&b, q.Subtotal)
	b.WriteString("fees\n")
	for _, f := range q.Fees {
		writeLine(&b, f)
	}
	b.WriteString("discounts\n")
	for _, d := range q.Discounts {
		writeLine(&b, d)
	}
	writeMoney(&b, q.Total)
	b.WriteString(q.IssuedAt)
	b.WriteByte('\n')
	b.WriteString(q.ExpiresAt)
	b.WriteByte('\n')
	return []byte(b.String())
}

func writeMoney(b *strings.Builder, m money) {
	b.WriteString(strconv.FormatInt(m.Amount, 10))
	b.WriteByte(':')
	b.WriteString(m.Currency)
	b.WriteByte('\n')
}

func writeLine(b *strings.Builder, l lineItem) {
	b.WriteString(l.Type)
	b.WriteByte(':')
	b.WriteString(strconv.FormatInt(l.Amount, 10))
	b.WriteByte(':')
	b.WriteString(l.Currency)
	b.WriteByte('\n')
}
