#!/usr/bin/env bash
# smoke.sh — minimal end-to-end check of the booted empty stack. Verifies the
# gateway is healthy, the placeholder is healthy, and the gateway proxies
# /placeholder/* to the placeholder. Exits non-zero on any failure so CI and
# `make smoke` can gate on it.
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
PLACEHOLDER_URL="${PLACEHOLDER_URL:-http://localhost:8081}"
PAYMENT_SIM_URL="${PAYMENT_SIM_URL:-http://localhost:8091}"
MAP_SIM_URL="${MAP_SIM_URL:-http://localhost:8092}"
NOTIFY_SINK_URL="${NOTIFY_SINK_URL:-http://localhost:8093}"

fail=0

expect_ok() {
  local desc="$1" url="$2" needle="$3"
  local body
  if ! body="$(curl -fsS --max-time 5 "$url" 2>/dev/null)"; then
    echo "FAIL: $desc — request to $url failed"
    fail=$((fail + 1))
    return
  fi
  if [[ "$body" == *"$needle"* ]]; then
    echo "PASS: $desc"
  else
    echo "FAIL: $desc — expected '$needle' in: $body"
    fail=$((fail + 1))
  fi
}

echo "smoke: gateway=$GATEWAY_URL placeholder=$PLACEHOLDER_URL"
expect_ok "gateway /healthz"           "$GATEWAY_URL/healthz"          '"status":"ok"'
expect_ok "placeholder /healthz"       "$PLACEHOLDER_URL/healthz"      '"service":"placeholder"'
expect_ok "gateway -> /placeholder/*"  "$GATEWAY_URL/placeholder/healthz" '"service":"placeholder"'

# S-T7 fake providers healthy + a one-shot behavioural check each.
expect_ok "payment-sim /healthz"       "$PAYMENT_SIM_URL/healthz"       '"service":"payment-sim"'
expect_ok "map-sim /healthz"           "$MAP_SIM_URL/healthz"           '"service":"map-sim"'
expect_ok "notify-sink /healthz"       "$NOTIFY_SINK_URL/healthz"       '"service":"notify-sink"'

expect_post() { # desc url data needle
  local desc="$1" url="$2" data="$3" needle="$4" body
  if ! body="$(curl -fsS --max-time 5 -X POST -H 'Content-Type: application/json' -d "$data" "$url" 2>/dev/null)"; then
    # non-2xx (e.g. a decline/timeout) still returns a body via -s; capture it
    body="$(curl -sS --max-time 5 -X POST -H 'Content-Type: application/json' -d "$data" "$url" 2>/dev/null)"
  fi
  if [[ "$body" == *"$needle"* ]]; then echo "PASS: $desc"; else echo "FAIL: $desc — expected '$needle' in: $body"; fail=$((fail + 1)); fi
}

expect_post "payment-sim authorizes a good card" \
  "$PAYMENT_SIM_URL/v1/psp/authorize" '{"card_number":"4111111111111111","amount":{"amount":42550,"currency":"THB"}}' '"status":"AUTHORIZED"'
expect_post "payment-sim declines ...0002" \
  "$PAYMENT_SIM_URL/v1/psp/authorize" '{"card_number":"4000000000000002","amount":{"amount":42550,"currency":"THB"}}' '"code":"PSP_CARD_DECLINED"'
expect_post "map-sim deterministic route" \
  "$MAP_SIM_URL/v1/route" '{"from":{"lat":13.7563,"lng":100.5018},"to":{"lat":13.7460,"lng":100.5340},"mode":"CAR"}' '"distance_m"'
expect_post "notify-sink captures a message" \
  "$NOTIFY_SINK_URL/v1/send" '{"channel":"PUSH","recipient":"usr_smoke","subject":"hi"}' '"message_id"'
expect_ok  "notify-sink inbox has it"  "$NOTIFY_SINK_URL/v1/inbox?recipient=usr_smoke" '"count":1'

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "smoke: all checks passed"
else
  echo "smoke: $fail check(s) failed"
fi
[ "$fail" -eq 0 ]
