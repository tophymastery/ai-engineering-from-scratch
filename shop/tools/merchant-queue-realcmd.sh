#!/usr/bin/env bash
# merchant-queue-realcmd.sh — the real_cmd launcher for the merchant-queue slot in
# the shared E2E env (S-T8 swap machinery), used by V-T11. Like
# tools/order-realcmd.sh it boots the ACTUAL merged service (not a contract stub),
# so the CQRS incoming-order projection, the kitchen-capacity admission control,
# and the accept→saga path run against the real merchant-queue store.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it. merchant_queue_v1 is forced
# ON here (FLAG_MERCHANT_QUEUE_V1=true) so the E2E env demos the slice "with the
# flag on". ORDER_URL points at the order slot so an admitted accept drives the
# real order saga (order.accepted).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/merchant-queue"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/merchant-queue" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-merchant-queue}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_MERCHANT_QUEUE_V1="${FLAG_MERCHANT_QUEUE_V1:-true}" \
  ORDER_URL="${ORDER_URL:-http://localhost:8105}" "$BIN"
