# Runbook — HMAC quote-signing-key rotation (pricing-promo, V-T8 / D10)

**Owner:** Growth · **Service:** `pricing-promo` · **Decision:** D10
**Rehearsed by:** `tools/rotate-quote-keys-demo.sh` (live) and
`services/pricing-promo` `TestKeyRotationRunbook` (unit) — both run in CI.

## What & why

A quote is priced once, signed, and lives in the Redis-like tier for a **10-min
TTL**; at checkout the presented quote is verified by HMAC over its canonical
body + expiry (D10 — "signed so checkout can verify integrity"). Verification
resolves the quote's `kid` to a signing secret held in the pricing key ring;
there is **no call back to a signer on the checkout hot path**. Rotation
therefore has one hard rule:

> **Publish (add) the new key *before* signing with it, and retire the old key
> only *after* every quote it signed has expired (≥ 10 min TTL).**

The key ring carries **at most 2 keys** so there is always exactly one overlap
window. Signing always uses the `primary` key; verification accepts **any** key
in the ring, so a quote signed by the outgoing key keeps verifying through the
overlap.

## SLOs this protects

| SLO | Target | Dashboard / alert |
|---|---|---|
| Quote latency | p99 < 300 ms | `deploy/dashboards/pricing.json` p1 · `PricingQuoteLatencyHigh` |
| Checkout integrity (tamper/expiry → 422) | 100% rejection; low steady 422 rate | p2 · `PricingQuoteRejectRateHigh` |
| Signing-key freshness | rotate ≤ 30 d | p5 · `PricingSigningKeyStale` |
| PG writes at checkout only | ≈ 1/50th of quotes | p4 · `PricingPGWriteRateAnomalous` |

## Rotation procedure

Endpoints are ops-only (non-prod / admin plane; disabled when `ENV=prod` in the
public build — they return `403 PRICING_ADMIN_DISABLED`). In production drive
them from the Growth control plane / secret store.

1. **Pre-flight.** Confirm checkouts are healthy: `PricingQuoteRejectRateHigh` is
   quiet. Note the current `primary_kid` from `GET /healthz`.

2. **Add key B (publish before signing).**
   `POST /v1/pricing/keys:rotate` → generates key B, adds it to the ring, makes
   it the **primary signer**. The ring now holds **A + B**.
   - Verify: `GET /healthz` shows `key_count: 2` and a new `primary_kid` (B).
   - New quotes are now signed with B; quotes still in flight were signed with A.

3. **Overlap window (do nothing for ≥ 10 min).** Quotes minted before step 2 were
   signed by **A** and MUST keep verifying at checkout — A is still in the ring.
   This is the no-broken-checkout guarantee. Wait until the longest-lived
   A-signed quote has expired (TTL = 10 min; wait 12 min for safety).

4. **Retire key A.** `POST /v1/pricing/keys:retire` → drops the oldest key (A).
   The ring now holds **B only**. Refuses to retire the primary or the last key
   (`400 VALIDATION`). Any quote still presenting kid A (all long since expired)
   now fails verification with **422 QUOTE_INVALID** — correct, since those
   quotes are past their 10-min TTL anyway.
   - Verify: `GET /healthz` shows `key_count: 1`, `primary_kid == B`;
     `PricingQuoteRejectRateHigh` did **not** spike.

5. **Post-checks.** `primary_kid == B` on `/healthz`; checkout 422 rate flat;
   quote p99 still < 300 ms.

### Rollback

If step 2 or 3 shows a spike in `PricingQuoteRejectRateHigh` (a bad key
distribution, or an edge/replica that didn't pick up B): **do not retire
anything.** Because A is still in the ring and primary-eligible, re-rotating to a
fresh B (or investigating the secret distribution) restores service. Retiring is
the only irreversible step and is gated behind the 10-min wait — retiring A
before the overlap elapses is the one mistake that invalidates in-flight quotes
(they lose their verifier → 422 at checkout → the customer must re-quote).

## Signer/store outage (D10 invariant)

If the pricing key material is **unavailable** for *new* quotes: new `POST
/v1/quotes` fail, but **already-issued quotes keep verifying at checkout** as
long as their `kid` is still in the ring and within the 10-min TTL. A Redis-tier
(quote-cache) flush merely forces a **re-quote** — it never loses a durable row,
because durable rows exist only for quotes that reached checkout (persisted to
PG). Do not fail checkouts for quotes whose signature + expiry still verify.
