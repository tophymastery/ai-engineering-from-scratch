#!/usr/bin/env bash
# feed-cache-realcmd.sh — the real_cmd launcher for the `feed-cache` slot in the
# shared E2E env (S-T8 swap machinery), used by V-T6. It boots the ACTUAL
# feed-cache service (not a contract stub) so the geo-tile feed cache (stale-
# while-revalidate) + merchant-page two-tier cache (singleflight over Redis) demo
# runs against the real in-process cache tiers (services/feed-cache/cache).
#
# V-T6 fronts the discovery read path (D11/D17):
#   - the browse feed origin is the RANKING slot (D17 two-phase: ranking re-ranks
#     the search top-500), so ORIGIN_FEED_URL points at the ranking port (8115);
#     the gateway routes /customer-bff/v1/customer/home -> feed-cache -> ranking
#     -> search.
#   - the merchant-page origin is MERCHANT-CATALOG (8102), so ORIGIN_MERCHANT_URL
#     points there; the gateway routes /customer-bff/v1/customer/merchants/* here.
#
# TTLs are shortened here (fresh 1s / stale 10s feed; L1 1s / L2 10s merchant) so
# the smoke can exercise the stale-while-revalidate transition (MISS -> HIT ->
# STALE + background revalidation) within a few seconds without wall-clock waits.
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment.
# feed_cache is forced ON so the E2E env demos "flag on"; the smoke also exercises
# the bypass path via the X-Flag-Override header (honoured only when testhooks are
# COMPILED IN, so this NON-PROD e2e binary is built with `-tags testhooks` —
# dev/preview/staging/e2e are testhooks builds by design; only prod compiles them
# out, enforced by ci/backdoor-scan.sh on prod builds, never on this one).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/feed-cache"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/feed-cache" && "$GO" build -tags testhooks -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-feed-cache}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_FEED_CACHE="${FLAG_FEED_CACHE:-true}" \
  ORIGIN_FEED_URL="${ORIGIN_FEED_URL:-http://localhost:8115}" \
  ORIGIN_MERCHANT_URL="${ORIGIN_MERCHANT_URL:-http://localhost:8102}" \
  FEED_FRESH_TTL="${FEED_FRESH_TTL:-1s}" FEED_STALE_TTL="${FEED_STALE_TTL:-10s}" \
  MERCHANT_L1_TTL="${MERCHANT_L1_TTL:-1s}" MERCHANT_L2_TTL="${MERCHANT_L2_TTL:-10s}" \
  "$BIN"
