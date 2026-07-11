package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// store_test.go — V-T8 property #3: PG persistence happens ONLY at checkout.
// POST /v1/quotes must write ZERO rows to the durable `quotes` table (the live
// quote lives only in the Redis-like TTL tier); the checkout path writes exactly
// one. Real DB-row-count assertions against the (SQLite-in-test) store.

// TestPGWritesOnlyAtCheckout is the headline row-count proof.
func TestPGWritesOnlyAtCheckout(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	ctx := context.Background()

	// Create SEVERAL quotes — none may touch PG.
	var quotes []Quote
	for i := 0; i < 5; i++ {
		_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "LUNCH25", true))
		if q.QuoteID == "" {
			t.Fatalf("quote %d not created", i)
		}
		quotes = append(quotes, q)
	}
	if n, _ := s.st.quoteRowCount(ctx); n != 0 {
		t.Fatalf("POST /v1/quotes wrote %d PG rows — want 0 (quotes live in Redis-like tier only)", n)
	}
	// The Redis-like tier DOES hold them.
	if st := s.cache.stats(); st["entries"] != 5 {
		t.Fatalf("quote cache entries = %d want 5", st["entries"])
	}

	// Check out ONE quote → exactly one PG row.
	qb, _ := json.Marshal(quotes[0])
	code, _ := do(t, h, "POST", "/v1/quotes/"+quotes[0].QuoteID+":checkout", string(qb))
	if code != 200 {
		t.Fatalf("checkout -> %d", code)
	}
	if n, _ := s.st.quoteRowCount(ctx); n != 1 {
		t.Fatalf("after 1 checkout, PG rows = %d want 1", n)
	}
	ok, _ := s.st.checkedOut(ctx, quotes[0].QuoteID)
	if !ok {
		t.Fatalf("checked-out quote %s not found in PG", quotes[0].QuoteID)
	}

	// Check out a SECOND distinct quote → two rows.
	qb2, _ := json.Marshal(quotes[1])
	do(t, h, "POST", "/v1/quotes/"+quotes[1].QuoteID+":checkout", string(qb2))
	if n, _ := s.st.quoteRowCount(ctx); n != 2 {
		t.Fatalf("after 2 checkouts, PG rows = %d want 2", n)
	}

	// Re-checkout the FIRST (double-tap / retry) → still 2 rows (idempotent).
	do(t, h, "POST", "/v1/quotes/"+quotes[0].QuoteID+":checkout", string(qb))
	if n, _ := s.st.quoteRowCount(ctx); n != 2 {
		t.Fatalf("re-checkout created a duplicate row: PG rows = %d want 2", n)
	}
}

// TestQuoteCacheTTLExpiry proves the Redis-like tier honours the 10-min TTL: a
// quote is a cache HIT before TTL and a MISS after (a Redis EX expiry), forcing a
// re-quote — and this never involves a PG write.
func TestQuoteCacheTTLExpiry(t *testing.T) {
	s, clk, _ := newTestServer(t)
	h := s.mux()
	_, q := doQuote(t, h, "POST", "/v1/quotes", createBody(tCart, 40000, "THB", "", true))

	if code, _ := do(t, h, "GET", "/v1/quotes/"+q.QuoteID, ""); code != 200 {
		t.Fatalf("fresh quote GET -> %d want 200", code)
	}
	clk.Advance(10 * time.Minute) // reach the TTL horizon
	code, m := do(t, h, "GET", "/v1/quotes/"+q.QuoteID, "")
	if code != 404 || errCode(m) != "QUOTE_NOT_FOUND" {
		t.Fatalf("expired-from-cache GET -> %d %s (want 404 QUOTE_NOT_FOUND)", code, errCode(m))
	}
	// Still zero PG rows — nothing was ever persisted without a checkout.
	if n, _ := s.st.quoteRowCount(context.Background()); n != 0 {
		t.Fatalf("PG rows = %d want 0 (no checkout occurred)", n)
	}
}
