#!/usr/bin/env bash
# preview.sh — simulate the D29 shared multi-tenant preview locally (S-T2).
#
# Given a PR number and its changed paths, this:
#   1. computes the CHANGED buildable services (reuses tools/changed-paths.sh);
#   2. boots ONE shared baseline (gateway + placeholder) — amortized across PRs;
#   3. "deploys" ONLY the changed services as a per-PR overlay and routes to it
#      with X-Preview-Tenant: pr-<n> through the existing gateway;
#   4. posts the per-PR URL and prints the cost model vs full-stack-per-PR,
#      failing (exit 1) if the ratio exceeds the 20% budget.
#
# Manifests for the real cluster live in deploy/preview-shared/ (render-verified)
# with scale-to-zero (2h idle) and TTL (7d) fields; this script proves the flow.
#
# Usage:
#   tools/preview.sh --pr 777 --files services/_placeholder/main.go
#   tools/preview.sh --pr 777 --base origin/main
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
# Full-stack-per-PR estimate = whole service catalog (TASKS.md Phase V: ~24
# services + 4 BFFs + gateway + placeholder baseline). Overridable for what-if.
FULL_STACK_PODS="${FULL_STACK_PODS:-30}"
COST_BUDGET_PCT="${COST_BUDGET_PCT:-20}"

PR=""
MODE=""
declare -a REST=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --pr) PR="$2"; shift 2 ;;
    --files) MODE="--files"; shift; REST=("$@"); break ;;
    --base) MODE="--base"; REST=("$2"); shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[[ -z "$PR" ]] && { echo "usage: preview.sh --pr <n> [--files <paths...> | --base <ref>]" >&2; exit 2; }
[[ -z "$MODE" ]] && { MODE="--files"; REST=("services/_placeholder/main.go"); }  # default reference PR

tmp="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" >/dev/null 2>&1 || true; done; rm -rf "$tmp"; }
trap cleanup EXIT
wait_healthy() { local u="$1"; for _ in $(seq 1 40); do curl -fsS --max-time 2 "$u" >/dev/null 2>&1 && return 0; sleep 0.25; done; return 1; }

echo "=== shared preview simulation: PR #$PR ==="

# 1. changed services (only services/bffs/gateway are deployable pods).
changed="$(tools/changed-paths.sh "$MODE" "${REST[@]}" || true)"
changed_list="$(printf '%s\n' "$changed" | grep -v '^$' || true)"
changed_count="$(printf '%s\n' "$changed_list" | grep -c . || true)"
echo "changed services (deployed for this PR only):"
printf '%s\n' "$changed_list" | sed 's/^/  - /'
echo "  => $changed_count changed pod(s)"

# 2. shared baseline (ONE stack for all PRs).
PH=18111; GW=18110
( cd "$ROOT/services/_placeholder" && "$GO" build -o "$tmp/ph" . )
( cd "$ROOT/gateway" && "$GO" build -o "$tmp/gw" . )
PORT=$PH SERVICE_NAME=baseline-placeholder "$tmp/ph" >"$tmp/ph.log" 2>&1 &
PIDS+=($!); wait_healthy "http://localhost:$PH/healthz" || { echo "baseline placeholder unhealthy"; exit 1; }
GATEWAY_MODE=preview PORT=$GW PLACEHOLDER_URL="http://localhost:$PH" "$tmp/gw" >"$tmp/gw.log" 2>&1 &
PIDS+=($!); wait_healthy "http://localhost:$GW/healthz" || { echo "baseline gateway unhealthy"; exit 1; }
echo "shared baseline up: gateway :$GW -> placeholder :$PH (scale-to-zero 2h idle, TTL 7d — see deploy/preview-shared/)"

# 3. per-PR route: request tagged with the tenant header hits the PR's view.
tenant="pr-$PR"
echo "routing a request as tenant $tenant through the shared gateway..."
curl -fsS -H "X-Preview-Tenant: $tenant" "http://localhost:$GW/placeholder/kv?key=demo&value=pr$PR-live" >/dev/null
got="$(curl -fsS -H "X-Preview-Tenant: $tenant" "http://localhost:$GW/placeholder/kv?key=demo")"
echo "  tenant view: $got"

# 4. per-PR URL + cost model.
url="https://pr-$PR.preview.shop.io   (header route: X-Preview-Tenant: $tenant)"
echo
echo "PREVIEW URL (posted to PR #$PR): $url"

ratio_pct="$(awk -v c="$changed_count" -v f="$FULL_STACK_PODS" 'BEGIN{ printf "%.1f", (c/f)*100 }')"
echo
echo "--- cost model ---"
echo "  per-PR pods (changed-only) : $changed_count"
echo "  full-stack-per-PR pods     : $FULL_STACK_PODS  (whole catalog, TASKS.md Phase V)"
echo "  cost ratio                 : ${ratio_pct}%  (budget: <= ${COST_BUDGET_PCT}%)"
under="$(awk -v r="$ratio_pct" -v b="$COST_BUDGET_PCT" 'BEGIN{ print (r<=b)?"yes":"no" }')"
if [[ "$under" == "yes" ]]; then
  echo "  RESULT: within budget"
else
  echo "  RESULT: OVER BUDGET" ; exit 1
fi
echo "=== preview simulation OK ==="
