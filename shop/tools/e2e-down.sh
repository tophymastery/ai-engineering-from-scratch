#!/usr/bin/env bash
# e2e-down.sh — tear down the shared E2E topology booted by tools/e2e-up.sh.
# Reaps every tracked slot pid + the gateway (safe no-op if none are running).
# Leaves the runtime overlay (.run/e2e-overlay.yaml) in place so a subsequent
# e2e-up re-reads the same swaps; pass --reset to also clear the overlay.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="$ROOT/.run/e2e"

reset=0
[ "${1:-}" = "--reset" ] && reset=1

if [ -d "$RUN" ]; then
  for pidfile in "$RUN"/*.pid; do
    [ -e "$pidfile" ] || continue
    pid="$(cat "$pidfile" 2>/dev/null || true)"
    name="$(basename "$pidfile" .pid)"
    if [ -n "$pid" ] && kill -0 "$pid" >/dev/null 2>&1; then
      kill "$pid" >/dev/null 2>&1 || true
      echo "stopped $name (pid $pid)"
    fi
    rm -f "$pidfile"
  done
  rm -f "$RUN/mode"
fi

if [ "$reset" = 1 ]; then
  rm -f "$ROOT/.run/e2e-overlay.yaml"
  echo "overlay reset (all slots back to manifest modes)"
fi

echo "E2E env down."
