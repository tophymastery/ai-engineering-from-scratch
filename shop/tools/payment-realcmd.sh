#!/usr/bin/env bash
# payment-realcmd.sh — the real_cmd launcher for the payment slot in the shared
# E2E env (S-T8 swap machinery), used by V-T10. Like tools/order-realcmd.sh it
# boots the ACTUAL merged service (not a contract stub), so the D9 idempotent
# money mutations, the exactly-once webhook/order-event inbox, and the PSP adapter
# run against the real payment store in the shared topology.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it. payment_v1 is forced ON here
# (FLAG_PAYMENT_V1=true) so the E2E env demos the slice "with the flag on".
# PAYMENT_SIM_URL points at the payment-sim fake (the downstream PSP);
# PAYMENT_SELF_URL is the payment slot's routable address for async webhooks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/payment"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/payment" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-payment}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_PAYMENT_V1="${FLAG_PAYMENT_V1:-true}" \
  PAYMENT_SIM_URL="${PAYMENT_SIM_URL:-http://localhost:8091}" \
  PAYMENT_SELF_URL="${PAYMENT_SELF_URL:-http://localhost:$PORT}" "$BIN"
