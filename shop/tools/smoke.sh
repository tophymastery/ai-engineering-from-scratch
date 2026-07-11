#!/usr/bin/env bash
# smoke.sh — minimal end-to-end check of the booted empty stack. Verifies the
# gateway is healthy, the placeholder is healthy, and the gateway proxies
# /placeholder/* to the placeholder. Exits non-zero on any failure so CI and
# `make smoke` can gate on it.
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
PLACEHOLDER_URL="${PLACEHOLDER_URL:-http://localhost:8081}"

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

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "smoke: all checks passed"
else
  echo "smoke: $fail check(s) failed"
fi
[ "$fail" -eq 0 ]
