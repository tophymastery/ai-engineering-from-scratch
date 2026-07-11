#!/usr/bin/env bash
# gateway-strip_test.sh — D29 layers 2 & 3 (S-T2).
#
# Boots a PROD-MODE gateway (GATEWAY_MODE=prod) in front of the placeholder and
# asserts:
#   (2) strip: X-Test-Clock / X-Flag-Override sent to the gateway NEVER reach
#       upstream (placeholder /headers echoes them back empty).
#   (3) alert: the gateway emits a WARN line with code TESTHOOK_HEADER_STRIPPED
#       immediately (< 1 min — measured, trivially sub-second here).
#
# Also a control: a dev-mode gateway passes the header through (proving the
# strip is prod-gated, not accidental).
#
# Self-contained: builds prod binaries, runs on private ports, tears down.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
tmp="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" >/dev/null 2>&1 || true; done; rm -rf "$tmp"; }
trap cleanup EXIT

PH_PORT=18091
GW_PORT_PROD=18090
GW_PORT_DEV=18092
PH_URL="http://localhost:$PH_PORT"
GW_PROD="http://localhost:$GW_PORT_PROD"
GW_DEV="http://localhost:$GW_PORT_DEV"

fail=0
note() { echo "  $*"; }
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=$((fail + 1)); }

wait_healthy() {
  local url="$1"
  for _ in $(seq 1 40); do curl -fsS --max-time 2 "$url" >/dev/null 2>&1 && return 0; sleep 0.25; done
  return 1
}

echo "=== gateway strip + alert test (prod build, prod mode) ==="
# Prod build (no testhooks): proves strip works even with backdoors compiled out.
( cd "$ROOT/services/_placeholder" && "$GO" build -o "$tmp/placeholder" . )
( cd "$ROOT/gateway" && "$GO" build -o "$tmp/gateway" . )

PORT=$PH_PORT SERVICE_NAME=placeholder "$tmp/placeholder" >"$tmp/ph.log" 2>&1 &
PIDS+=($!)
wait_healthy "$PH_URL/healthz" || { echo "placeholder never healthy"; exit 1; }

GATEWAY_MODE=prod PORT=$GW_PORT_PROD PLACEHOLDER_URL="$PH_URL" "$tmp/gateway" >"$tmp/gw_prod.log" 2>&1 &
PIDS+=($!)
wait_healthy "$GW_PROD/healthz" || { echo "prod gateway never healthy"; exit 1; }

GATEWAY_MODE=dev PORT=$GW_PORT_DEV PLACEHOLDER_URL="$PH_URL" "$tmp/gateway" >"$tmp/gw_dev.log" 2>&1 &
PIDS+=($!)
wait_healthy "$GW_DEV/healthz" || { echo "dev gateway never healthy"; exit 1; }

# --- Layer 2: strip through prod gateway ---
t0=$(date +%s.%N)
resp="$(curl -fsS --max-time 5 \
  -H "X-Test-Clock: 2020-01-01T00:00:00Z" \
  -H "X-Flag-Override: risk_v1=off" \
  "$GW_PROD/placeholder/headers")"
note "upstream saw: $resp"
if [[ "$resp" == *'"X-Test-Clock":""'* && "$resp" == *'"X-Flag-Override":""'* ]]; then
  pass "prod gateway strips both backdoor headers before upstream"
else
  bad "backdoor header reached upstream in prod mode"
fi

# --- Layer 3: alert line emitted immediately ---
alert_ok=0
for _ in $(seq 1 20); do  # poll up to ~5s; alert is synchronous so this is instant
  if grep -q "TESTHOOK_HEADER_STRIPPED" "$tmp/gw_prod.log"; then alert_ok=1; break; fi
  sleep 0.25
done
t1=$(date +%s.%N)
elapsed="$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", b-a}')"
if [[ "$alert_ok" -eq 1 ]]; then
  n="$(grep -c "TESTHOOK_HEADER_STRIPPED" "$tmp/gw_prod.log")"
  pass "prod-log alert emitted ($n WARN line(s), code TESTHOOK_HEADER_STRIPPED) in ${elapsed}s (< 60s)"
else
  bad "no TESTHOOK_HEADER_STRIPPED alert in prod gateway log"
fi

# --- Control: dev-mode gateway does NOT strip (header passes through) ---
resp_dev="$(curl -fsS --max-time 5 -H "X-Test-Clock: 2020-01-01T00:00:00Z" "$GW_DEV/placeholder/headers")"
if [[ "$resp_dev" == *'"X-Test-Clock":"2020-01-01T00:00:00Z"'* ]]; then
  pass "control: dev-mode gateway passes header through (strip is prod-gated)"
else
  bad "dev-mode gateway unexpectedly altered the header: $resp_dev"
fi

echo "----"
[[ "$fail" -eq 0 ]] && echo "gateway-strip: all checks passed" || echo "gateway-strip: $fail check(s) failed"
[[ "$fail" -eq 0 ]]
