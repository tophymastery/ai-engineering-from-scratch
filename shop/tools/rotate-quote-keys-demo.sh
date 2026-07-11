#!/usr/bin/env bash
# rotate-quote-keys-demo.sh — REHEARSES docs/runbooks/quote-key-rotation.md
# end-to-end against the real pricing-promo service (V-T8 / D10).
#
# The invariant being proven (also asserted in the Go unit test
# services/pricing-promo TestKeyRotationRunbook):
#   1. add key B  -> ring holds BOTH A and B; new quotes are signed with B
#   2. OVERLAP    -> a quote signed by A STILL verifies (no broken checkout)
#   3. retire A   -> ring drops A; an A-signed quote no longer verifies (422),
#                    a B-signed quote still does (200) and checks out.
#
# The verify probe is GET /v1/quotes/{id} (runs the same HMAC+expiry gate as
# checkout, non-destructive). Exits nonzero on any failed assertion.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
BIN="$(mktemp -d)"
PPORT=18207
PIDS=()
fail=0
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$BIN"; }
trap cleanup EXIT

step=0
ok()  { step=$((step+1)); printf '  PASS [%02d] %s\n' "$step" "$1"; }
no()  { step=$((step+1)); printf '  FAIL [%02d] %s\n' "$step" "$1"; fail=$((fail+1)); }

jval() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p'; }
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$@" 2>/dev/null; }

echo "== build pricing-promo =="
( cd "$ROOT/services/pricing-promo" && "$GO" build -o "$BIN/pricing-promo" . )

echo "== boot pricing-promo (:$PPORT, pricing_v1 on, admin enabled) =="
PORT="$PPORT" SERVICE_NAME=pricing-promo ENV=e2e FLAG_PRICING_V1=true "$BIN/pricing-promo" >"$BIN/p.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 40); do curl -fsS --max-time 1 "http://localhost:$PPORT/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

Q='{"cart_id":"crt_rotate","subtotal":{"amount":40000,"currency":"THB"},"voucher_code":"LUNCH25","delivery_location":{"lat":13.7,"lng":100.5}}'

KIDA="$(curl -s "http://localhost:$PPORT/healthz" | jval primary_kid)"
[ -n "$KIDA" ] && ok "pricing up; primary key A (kid=$KIDA)" || no "no primary kid"

# --- quote under key A ---
QA="$(curl -s -X POST "http://localhost:$PPORT/v1/quotes" -H 'Content-Type: application/json' -d "$Q")"
QIDA="$(printf '%s' "$QA" | jval quote_id)"; QKIDA="$(printf '%s' "$QA" | jval kid)"
[ -n "$QIDA" ] && [ "$QKIDA" = "$KIDA" ] && ok "quote signed under key A (quote_id=$QIDA)" || no "quote A not signed with A (kid=$QKIDA)"
c="$(code "http://localhost:$PPORT/v1/quotes/$QIDA")"
[ "$c" = 200 ] && ok "quote A verifies (GET 200)" || no "quote A verify -> $c"

# --- STEP 1: rotate (add key B, sign new quotes with B) ---
R="$(curl -s -X POST "http://localhost:$PPORT/v1/pricing/keys:rotate" -H 'Content-Type: application/json' -d '{}')"
KIDB="$(printf '%s' "$R" | jval primary_kid)"
[ -n "$KIDB" ] && [ "$KIDB" != "$KIDA" ] && ok "rotated: new primary key B (kid=$KIDB)" || no "rotate did not add a distinct key"
KC="$(curl -s "http://localhost:$PPORT/healthz" | sed -n 's/.*"key_count":\([0-9]*\).*/\1/p')"
[ "$KC" = 2 ] && ok "ring holds BOTH keys (A+B)" || no "key_count=$KC want 2"

# --- new quote signed with B ---
QB="$(curl -s -X POST "http://localhost:$PPORT/v1/quotes" -H 'Content-Type: application/json' -d "$Q")"
QIDB="$(printf '%s' "$QB" | jval quote_id)"; QKIDB="$(printf '%s' "$QB" | jval kid)"
[ "$QKIDB" = "$KIDB" ] && ok "new quotes are signed with key B" || no "new quote kid=$QKIDB want $KIDB"

# --- STEP 2: OVERLAP — both A and B verify ---
c="$(code "http://localhost:$PPORT/v1/quotes/$QIDA")"
[ "$c" = 200 ] && ok "OVERLAP: A-signed quote STILL verifies during rotation ($c)" || no "quote A rejected during overlap -> $c"
c="$(code "http://localhost:$PPORT/v1/quotes/$QIDB")"
[ "$c" = 200 ] && ok "B-signed quote verifies ($c)" || no "quote B -> $c"

# --- STEP 3: retire A (safe only once all A-signed quotes have expired ≥10min) ---
RT="$(curl -s -X POST "http://localhost:$PPORT/v1/pricing/keys:retire" -H 'Content-Type: application/json' -d '{}')"
RETIRED="$(printf '%s' "$RT" | jval retired_kid)"
[ "$RETIRED" = "$KIDA" ] && ok "retired key A (kid=$KIDA)" || no "retire removed wrong kid ($RETIRED)"
KC="$(curl -s "http://localhost:$PPORT/healthz" | sed -n 's/.*"key_count":\([0-9]*\).*/\1/p')"
[ "$KC" = 1 ] && ok "ring drops A — 1 key (B) remains" || no "key_count=$KC want 1 after retire"

# After retire, an A-signed quote no longer verifies (kid gone) -> 422.
c="$(code "http://localhost:$PPORT/v1/quotes/$QIDA")"
[ "$c" = 422 ] && ok "retired key-A quote no longer verifies (422)" || no "A-signed quote after retire -> $c want 422"
# B-signed quote still verifies and checks out.
c="$(code "http://localhost:$PPORT/v1/quotes/$QIDB")"
[ "$c" = 200 ] && ok "B-signed quote still verifies after retire ($c)" || no "quote B after retire -> $c"
c="$(code -X POST "http://localhost:$PPORT/v1/quotes/$QIDB:checkout" -H 'Content-Type: application/json' -d "$QB")"
[ "$c" = 200 ] && ok "B-signed quote checks out under the new key ($c)" || no "checkout B -> $c want 200"

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "rotate-quote-keys-demo: GREEN — $step/$step assertions; quote-key-rotation runbook rehearsed (A→B overlap→retire A)"
  exit 0
else
  echo "rotate-quote-keys-demo: RED — $fail of $step assertions failed"; exit 1
fi
