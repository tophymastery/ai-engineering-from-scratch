#!/usr/bin/env bash
# preview-isolation_test.sh — cross-PR isolation for the shared preview (S-T2).
#
# Boots ONE shared baseline (gateway -> placeholder) and drives two simulated
# previews, pr-101 and pr-102, both mutating the SAME entity type ("order")
# through the gateway with a per-PR tenant header (X-Preview-Tenant: pr-<n>).
# Asserts ZERO data bleed: each tenant reads back only its own write, and a
# third tenant that never wrote reads empty.
#
# This is the D29 "run_id isolation" property proven against a shared stack —
# no full-stack-per-PR needed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
tmp="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" >/dev/null 2>&1 || true; done; rm -rf "$tmp"; }
trap cleanup EXIT

PH_PORT=18101
GW_PORT=18100
PH_URL="http://localhost:$PH_PORT"
GW_URL="http://localhost:$GW_PORT"

fail=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=$((fail + 1)); }

wait_healthy() { local u="$1"; for _ in $(seq 1 40); do curl -fsS --max-time 2 "$u" >/dev/null 2>&1 && return 0; sleep 0.25; done; return 1; }

# KV helpers routed THROUGH the gateway with a tenant header.
kv_set() { curl -fsS --max-time 5 -H "X-Preview-Tenant: $1" "$GW_URL/placeholder/kv?key=$2&value=$3"; }
kv_get() { curl -fsS --max-time 5 -H "X-Preview-Tenant: $1" "$GW_URL/placeholder/kv?key=$2"; }
val_of() { sed -n 's/.*"value":"\([^"]*\)".*/\1/p' <<<"$1"; }

echo "=== shared-preview cross-PR isolation test ==="
( cd "$ROOT/services/_placeholder" && "$GO" build -o "$tmp/placeholder" . )
( cd "$ROOT/gateway" && "$GO" build -o "$tmp/gateway" . )

echo "booting ONE shared baseline (1 gateway + 1 placeholder) for BOTH previews..."
PORT=$PH_PORT SERVICE_NAME=placeholder "$tmp/placeholder" >"$tmp/ph.log" 2>&1 &
PIDS+=($!); wait_healthy "$PH_URL/healthz" || { echo "placeholder unhealthy"; exit 1; }
GATEWAY_MODE=preview PORT=$GW_PORT PLACEHOLDER_URL="$PH_URL" "$tmp/gateway" >"$tmp/gw.log" 2>&1 &
PIDS+=($!); wait_healthy "$GW_URL/healthz" || { echo "gateway unhealthy"; exit 1; }

# Two PRs mutate the SAME entity type "order" concurrently against the baseline.
kv_set pr-101 order alpha >/dev/null
kv_set pr-102 order beta  >/dev/null

v101="$(val_of "$(kv_get pr-101 order)")"
v102="$(val_of "$(kv_get pr-102 order)")"
v999="$(val_of "$(kv_get pr-999 order)")"  # never wrote

echo "  pr-101 reads order=[$v101]  pr-102 reads order=[$v102]  pr-999 reads order=[$v999]"

[[ "$v101" == "alpha" ]] && pass "pr-101 sees its own write (alpha)" || bad "pr-101 expected alpha, got [$v101]"
[[ "$v102" == "beta"  ]] && pass "pr-102 sees its own write (beta)"  || bad "pr-102 expected beta, got [$v102]"
[[ "$v101" != "$v102" ]] && pass "no bleed: pr-101 and pr-102 diverge on same entity type" || bad "DATA BLEED: both tenants read [$v101]"
[[ -z "$v999" ]] && pass "no bleed: uninvolved tenant pr-999 reads empty" || bad "bleed to pr-999: [$v999]"

echo "----"
[[ "$fail" -eq 0 ]] && echo "preview-isolation: zero cross-PR bleed (2 tenants, shared baseline)" || echo "preview-isolation: $fail check(s) failed"
[[ "$fail" -eq 0 ]]
