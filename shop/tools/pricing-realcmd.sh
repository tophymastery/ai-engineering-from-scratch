#!/usr/bin/env bash
# pricing-realcmd.sh — the real_cmd launcher for the pricing-promo slot in the
# shared E2E env (S-T8 swap machinery), used by V-T8. Like tools/cart-realcmd.sh
# it boots the ACTUAL merged service (not a contract stub), so the quote engine
# (typed fees[]/discounts[], HMAC-signed quote) and the tampered/expired-quote →
# 422 checkout gate run against the real pricing store.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it. pricing_v1 is forced ON here
# (FLAG_PRICING_V1=true) so the E2E env demos the slice "with the flag on".
# CART_URL points at the cart slot (V-T7): when a quote request omits an explicit
# subtotal, pricing CONSUMES the cart contract there (the pricing-promo→cart pact)
# for the authoritative subtotal.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/pricing-promo"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/pricing-promo" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-pricing-promo}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_PRICING_V1="${FLAG_PRICING_V1:-true}" \
  CART_URL="${CART_URL:-http://localhost:8104}" \
  QUOTE_TTL="${QUOTE_TTL:-10m}" "$BIN"
