#!/usr/bin/env bash
# ranking-realcmd.sh — the real_cmd launcher for the `ranking` slot in the shared
# E2E env (S-T8 swap machinery), used by V-T5. It boots the ACTUAL ranking service
# (not a contract stub) so the browse-feed re-rank demo runs against the real
# in-process feature store + auto-fallback breaker (services/ranking/rank).
#
# D17 is two-phase: search RETRIEVES the top-500 (the search slot), ranking
# RE-RANKS to the top-50. So this launcher points the ranking service at the search
# slot via SEARCH_URL (default the E2E search port 8103). The gateway routes the
# browse BFF endpoint (/customer-bff/v1/customer/home) to this slot; geo search
# stays on search-query. e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the
# environment. ranking_ml is forced ON here so the E2E env demos "flag on"; the
# smoke also exercises "flag off" via the X-Flag-Override header. That per-request
# override is honoured only when testhooks are COMPILED IN, so this NON-PROD e2e
# binary is built with `-tags testhooks` (dev/preview/staging/e2e are testhooks
# builds by design — only prod compiles them out; ci/backdoor-scan.sh enforces that
# on prod builds, never on this one). The gateway (dev mode) passes the header
# through untouched.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/ranking"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/ranking" && "$GO" build -tags testhooks -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-ranking}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_RANKING_ML="${FLAG_RANKING_ML:-true}" \
  SEARCH_URL="${SEARCH_URL:-http://localhost:8103}" "$BIN"
