#!/usr/bin/env bash
# e2e-swap.sh <service> [real_cmd] — flip ONE slot from stub to real in the shared
# E2E env (S-T8) WITHOUT editing the manifest:
#   1. record the swap in the runtime overlay (.run/e2e-overlay.yaml)
#   2. kill just that slot's process
#   3. relaunch it from real_cmd on the SAME port
#   4. healthcheck; the gateway keeps routing (upstream port is unchanged)
# and print the wall-time of the swap (DoD budget < 15 min; expect seconds here).
#
# Default real_cmd is the documented contract-server alias (tools/e2e-realcmd.sh);
# pass an explicit real_cmd (e.g. a merged slice binary path) as the 2nd arg.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="$ROOT/.run/e2e"
BIN="$RUN/bin"
LOG="$RUN/log"
TOPO="$ROOT/deploy/e2e/topology.yaml"
OVERLAY="$ROOT/.run/e2e-overlay.yaml"
E2ECTL="$BIN/e2ectl"

svc="${1:?usage: e2e-swap.sh <service> [real_cmd]}"
real_cmd="${2:-bash tools/e2e-realcmd.sh}"

[ -x "$E2ECTL" ] || { echo "e2e-swap: env not up (run make e2e-up first)"; exit 1; }
export STUBGEN="$BIN/stubgen"

now_ms() { date +%s%3N; }
start="$(now_ms)"

# 1. record the swap in the overlay (single source of the RUNTIME state).
"$E2ECTL" set-overlay "$OVERLAY" "$svc" real "$real_cmd"

# Resolve the slot's port + contract AFTER the overlay is applied.
line="$("$E2ECTL" plan "$TOPO" "$OVERLAY" | awk -F'\t' -v s="$svc" '$1==s')"
[ -n "$line" ] || { echo "e2e-swap: no slot named $svc"; exit 1; }
port="$(echo "$line" | cut -f2)"
contract="$(echo "$line" | cut -f4)"

echo "e2e-swap: $svc stub -> real (port $port, real_cmd: $real_cmd)"

# 2. kill the current slot process.
if [ -f "$RUN/$svc.pid" ]; then
  pid="$(cat "$RUN/$svc.pid")"
  kill "$pid" >/dev/null 2>&1 || true
  for _ in $(seq 1 40); do kill -0 "$pid" >/dev/null 2>&1 || break; sleep 0.05; done
fi

# 3. relaunch from real_cmd on the same port (same launch path e2e-up uses).
PORT="$port" CONTRACT="$contract" SERVICE_NAME="$svc" \
  bash -c "$real_cmd" > "$LOG/$svc.log" 2>&1 &
echo $! > "$RUN/$svc.pid"

# 4. healthcheck the swapped slot.
ok=0
for _ in $(seq 1 120); do
  curl -fsS --max-time 2 "http://localhost:$port/healthz" >/dev/null 2>&1 && { ok=1; break; }
  sleep 0.1
done
[ "$ok" = 1 ] || { echo "e2e-swap: $svc did not become healthy after swap"; tail -n 8 "$LOG/$svc.log"; exit 1; }

# Confirm the gateway still routes to it (no gateway restart needed).
curl -fsS --max-time 2 "http://localhost:${GATEWAY_PORT:-8080}/$svc/healthz" >/dev/null 2>&1 \
  && route_ok="yes" || route_ok="NO"

end="$(now_ms)"
ms=$((end - start))
echo "e2e-swap: $svc is REAL and healthy in ${ms} ms (gateway still routing: $route_ok)"
echo "SWAP_WALL_MS=$ms"
