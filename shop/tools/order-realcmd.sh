#!/usr/bin/env bash
# order-realcmd.sh — the real_cmd launcher for the order slot in the shared E2E
# env (S-T8 swap machinery), used by V-T9. Like tools/cart-realcmd.sh /
# tools/pricing-realcmd.sh it boots the ACTUAL merged service (not a contract
# stub), so the full order state machine, the durable-timer sweeper, the D9
# idempotent checkout, and the exactly-once inbox run against the real order
# store in the shared topology.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it. saga_v1 is forced ON here
# (FLAG_SAGA_V1=true) so the E2E env demos the slice "with the flag on".
# PAYMENT_URL points at the payment slot (V-T10 / the payment-sim fake) for the
# authorize/capture/void saga steps. SWEEP_TICK is short so timer-driven
# transitions (remediation / T_accept / T_dispatch / capture-by) are observable.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/order"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/order" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-order}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_SAGA_V1="${FLAG_SAGA_V1:-true}" \
  PAYMENT_URL="${PAYMENT_URL:-http://localhost:8091}" \
  SWEEP_TICK="${SWEEP_TICK:-2s}" "$BIN"
