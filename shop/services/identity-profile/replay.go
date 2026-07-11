package main

import (
	"context"
	"errors"
	"fmt"
)

// replay.go — proves the D3 payoff: because order snapshots/events carry
// usr_/adr_ TOKENS only (never PII), an order can be reconstructed from its
// immutable history WITHOUT reading any personal data. So erasure (which shreds
// the PII key) does NOT break order replay — the two concerns are decoupled.

// lineItem is a token/price-only order line (no PII).
type lineItem struct {
	SKU        string `json:"sku"`
	Qty        int    `json:"qty"`
	PriceMinor int64  `json:"price_minor"`
}

// orderSnapshot is an order event exactly as it sits on the order service's
// append-only log: only token references and money. This is the shape D3
// mandates ("all events and order snapshots carry usr_/adr_ tokens only").
type orderSnapshot struct {
	OrderToken   string     `json:"order_token"`
	UserToken    string     `json:"user_token"`
	AddrToken    string     `json:"addr_token"`
	Jurisdiction string     `json:"jurisdiction"`
	Items        []lineItem `json:"items"`
	Currency     string     `json:"currency"`
}

// replayedOrder is the reconstructed order — token references resolved to their
// non-PII form, totals recomputed. Contains no personal data.
type replayedOrder struct {
	OrderToken string   `json:"order_token"`
	UserRef    tokenRef `json:"user_ref"`
	AddrRef    tokenRef `json:"addr_ref"`
	TotalMinor int64    `json:"total_minor"`
	Currency   string   `json:"currency"`
	LineCount  int      `json:"line_count"`
}

var errUnknownCell = errors.New("no cell homes this jurisdiction")

// replayOrder reconstructs an order purely from its token-only snapshot. It
// resolves the usr_/adr_ tokens to non-PII references (confirming they are real,
// in-cell references) and recomputes the total. It NEVER decrypts PII, so it
// succeeds identically before and after the user's PII has been crypto-shredded.
func replayOrder(ctx context.Context, s *stores, snap orderSnapshot) (replayedOrder, error) {
	cs := s.cell(snap.Jurisdiction)
	if cs == nil {
		return replayedOrder{}, fmt.Errorf("%w: %q", errUnknownCell, snap.Jurisdiction)
	}
	userRef, err := cs.resolveToken(ctx, snap.UserToken)
	if err != nil {
		return replayedOrder{}, err
	}
	addrRef, err := cs.resolveToken(ctx, snap.AddrToken)
	if err != nil {
		return replayedOrder{}, err
	}
	var total int64
	for _, it := range snap.Items {
		total += it.PriceMinor * int64(it.Qty)
	}
	return replayedOrder{
		OrderToken: snap.OrderToken,
		UserRef:    userRef,
		AddrRef:    addrRef,
		TotalMinor: total,
		Currency:   snap.Currency,
		LineCount:  len(snap.Items),
	}, nil
}
