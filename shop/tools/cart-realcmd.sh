#!/usr/bin/env bash
# cart-realcmd.sh — the real_cmd launcher for the cart slot in the shared E2E env
# (S-T8 swap machinery), used by V-T7. Like tools/merchant-catalog-realcmd.sh it
# boots the ACTUAL merged service (not a contract stub), so the add/remove/get +
# ETag concurrency demo (incl. the 412 stale write) and the menu-change
# revalidation demo run against the real cart store + Redis-like snapshot tier.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it. cart_v1 is forced ON here
# (FLAG_CART_V1=true) so the E2E env demos the slice "with the flag on". CATALOG_URL
# points at the merchant-catalog slot (add-time item validation, the cart pact);
# CART_SNAPSHOT_TTL is kept short so the freshness-window behaviour is observable.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/cart"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/cart" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-cart}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_CART_V1="${FLAG_CART_V1:-true}" \
  CATALOG_URL="${CATALOG_URL:-http://localhost:8102}" \
  CART_SNAPSHOT_TTL="${CART_SNAPSHOT_TTL:-5s}" "$BIN"
