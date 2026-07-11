#!/usr/bin/env bash
# dev-down.sh — tear down whatever dev-up.sh started (docker or process mode).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/.run"

mode="process"
[ -f "$RUN_DIR/mode" ] && mode="$(cat "$RUN_DIR/mode")"

if [ "$mode" = "docker" ] && docker info >/dev/null 2>&1; then
  echo "stopping docker-compose stack"
  docker compose -f "$ROOT/docker-compose.yml" down -v || true
fi

# Always reap any tracked processes (safe no-op if none / already gone).
for svc in gateway placeholder payment-sim map-sim notify-sink; do
  pidfile="$RUN_DIR/$svc.pid"
  if [ -f "$pidfile" ]; then
    pid="$(cat "$pidfile")"
    if kill -0 "$pid" >/dev/null 2>&1; then
      kill "$pid" >/dev/null 2>&1 || true
      echo "stopped $svc (pid $pid)"
    fi
    rm -f "$pidfile"
  fi
done

rm -f "$RUN_DIR/mode"
echo "stack down."
