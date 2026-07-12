#!/usr/bin/env bash
# dispatch-realcmd.sh — the real_cmd launcher for the dispatch slot in the shared
# E2E env (S-T8 swap machinery), used by V-T12. Like tools/order-realcmd.sh it
# boots the ACTUAL merged service (not a contract stub), so the zone-owned batch
# matcher, the exclusive driver reservations, the deterministic snapshot log, and
# the offer/accept path run against the real dispatch engine in the shared topology.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It builds
# the binary on demand (cached) and execs it. dispatch_batch is forced ON here
# (FLAG_DISPATCH_BATCH=true) so the E2E env demos the slice "with the flag on".
# ORDER_URL points at the order slot so an accepted offer drives the real order
# saga (dispatch.assigned → ACCEPTED→DISPATCHED). DISPATCH_TICK is short so the
# batch tick is observable; DISPATCH_SEED is pinned so replays are reproducible.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/dispatch"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/dispatch" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-dispatch}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_DISPATCH_BATCH="${FLAG_DISPATCH_BATCH:-true}" \
  ORDER_URL="${ORDER_URL:-http://localhost:8105}" \
  MAP_SIM_URL="${MAP_SIM_URL:-http://localhost:8092}" \
  DISPATCH_TICK="${DISPATCH_TICK:-500ms}" DISPATCH_SEED="${DISPATCH_SEED:-1}" \
  DISPATCH_PARTITIONS="${DISPATCH_PARTITIONS:-64}" "$BIN"
