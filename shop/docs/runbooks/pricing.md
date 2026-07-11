# Runbook — pricing-promo (V-T8)

Owner: **Growth** (see `ownership.yaml`). Service: `pricing-promo`, port 8107.
Flag: `pricing_v1` (ships dark; enable per environment). Decision: **D10**.

## What it does

The quote engine (D10). Prices a cart — items + typed **fees[]** (DELIVERY,
SERVICE, SURGE) + typed **discounts[]** (PROMO, VOUCHER) → total, in integer
minor units + ISO currency (02 §1 / §5) — via `POST /v1/quotes`, and returns an
**HMAC-signed** quote with a **10-min TTL**. The live quote lives in a Redis-like
TTL tier; **PG persistence happens only at checkout** (`POST
/v1/quotes/{id}:checkout`), which re-verifies the signed quote (tampered/expired
⇒ 422) before writing exactly one durable row.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Quote latency | p99 < 300 ms | `PricingQuoteLatencyHigh` |
| Checkout integrity (tamper/expiry → 422) | 100% rejection | `PricingQuoteRejectRateHigh` |
| PG writes at checkout only | ≈ 1/50th of quotes | `PricingPGWriteRateAnomalous` |
| Signing-key freshness | rotate ≤ 30 d | `PricingSigningKeyStale` |

## Key invariants

- **Deterministic pricing math.** fees/discounts/total are integer minor units
  only (never floats); surge is a function of the quote's issue time (a frozen
  clock reproduces it exactly). Same inputs + rate config ⇒ byte-identical quote.
- **Quotes are HMAC-signed; tamper/expiry ⇒ 422.** The signature covers the
  canonical quote body + `expires_at`, so any mutation of amounts, line items,
  the bound `cart_id`, or the expiry breaks the HMAC; an authentic-but-expired
  quote is rejected on the signed expiry. Both map to **422** — the correct
  rejection of an invalid quote, **not** a server error and **not** a charge.
- **PG is written ONLY at checkout.** `POST /v1/quotes` writes nothing durable;
  the quote lives in the Redis-like 10-min TTL tier. ~99% of quotes are never
  checked out, so the `quotes` table sees ~1/50th of pricing's request volume
  (D10). A Redis flush merely forces a re-quote; it never loses a durable row.
- **Checkout is idempotent on `quote_id`** (`INSERT … ON CONFLICT DO NOTHING`):
  a double "Pay" tap / BFF retry consuming the same quote never creates a second
  row.

## Alert actions

- **PricingQuoteLatencyHigh** — check the cart-read dependency (`CART_URL`) when
  a quote omits an explicit subtotal (pricing consumes the cart contract there);
  check the sign path. Falling back to a static rate config never affects
  correctness, only surge accuracy.
- **PricingQuoteRejectRateHigh** — a spike in checkout 422s after a key rotation
  means the outgoing key was retired before the 10-min overlap elapsed
  (in-flight quotes lost their verifier). See the rollback in
  `docs/runbooks/quote-key-rotation.md`. Otherwise it is stale quotes past TTL
  (customers re-quote) — no money impact.
- **PricingPGWriteRateAnomalous** — the quote path must not persist; a rising
  PG-write:quote ratio is a regression that 50×'s pricing's write load. Confirm
  only the checkout handler calls `persistAtCheckout`.
- **PricingSigningKeyStale** — run `docs/runbooks/quote-key-rotation.md`.

## Rollout

Ships dark (`FLAG_PRICING_V1=false`). Staging/preview overlays flip it on; prod
via a canary-gated rollout. The flag gates `POST /v1/quotes` (and checkout)
inside the service; disabled ⇒ `404 PRICING_DISABLED`.

## Sandbox adaptations (this environment)

Disclosed in `VERIFICATION.md §V-T8`. No Redis daemon ⇒ the 10-min TTL tier is an
in-process `quoteCache` with the same fresh/miss TTL contract under the injected
clock. No PG ⇒ in-memory SQLite in tests; the production schema is
`services/pricing-promo/migrations/0001_pricing.pg.sql`. HMAC keys are generated
in-process and rotated via the admin endpoints (a production deployment loads
seed secrets from the per-cell secret store keyed by kid). A literal sustained
10k RPS is unreachable in-sandbox, so the p99 budget is proven by measured per-op
p99 + a concurrency burst (V-T31 load harness fills the throughput seam). The
signing/verification, rotation, deterministic math, and PG-only-at-checkout LOGIC
is real and fully tested under `-race`.
