#!/usr/bin/env bash
# dev-up.sh — boot the empty stack (gateway + placeholder) and wait for health.
#
# Canonical definition is docker-compose.yml. If a working Docker daemon is
# present we use it; otherwise we fall back to a process-based boot that
# compiles and runs the two Go binaries directly (std-lib only, so this needs
# nothing but the Go toolchain) and health-checks them with curl. Either way
# the observable topology is identical: gateway on :8080 proxying
# /placeholder/* to the placeholder on :8081. See VERIFICATION.md.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/.run"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
PLACEHOLDER_URL="${PLACEHOLDER_URL:-http://localhost:8081}"
# S-T7 fake providers (03 §5).
PAYMENT_SIM_URL="${PAYMENT_SIM_URL:-http://localhost:8091}"
MAP_SIM_URL="${MAP_SIM_URL:-http://localhost:8092}"
NOTIFY_SINK_URL="${NOTIFY_SINK_URL:-http://localhost:8093}"
GO="${GO:-/usr/local/go/bin/go}"

mkdir -p "$RUN_DIR"

wait_healthy() {
  local url="$1" name="$2" tries=60
  for _ in $(seq 1 "$tries"); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      echo "  $name healthy ($url)"
      return 0
    fi
    sleep 0.5
  done
  echo "  ERROR: $name never became healthy at $url" >&2
  return 1
}

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  echo "docker daemon detected: booting via docker-compose"
  echo "docker" > "$RUN_DIR/mode"
  docker compose -f "$ROOT/docker-compose.yml" up -d --build
  wait_healthy "$GATEWAY_URL/healthz" gateway
  wait_healthy "$PLACEHOLDER_URL/healthz" placeholder
  wait_healthy "$PAYMENT_SIM_URL/healthz" payment-sim
  wait_healthy "$MAP_SIM_URL/healthz" map-sim
  wait_healthy "$NOTIFY_SINK_URL/healthz" notify-sink
  echo "stack up (docker)."
  exit 0
fi

echo "docker daemon unavailable: falling back to process-based boot"
echo "process" > "$RUN_DIR/mode"

# Build all binaries (placeholder + gateway + the three S-T7 fakes).
echo "building placeholder + gateway + fakes..."
( cd "$ROOT/services/_placeholder" && "$GO" build -o "$RUN_DIR/placeholder" . )
( cd "$ROOT/gateway" && "$GO" build -o "$RUN_DIR/gateway" . )
( cd "$ROOT/services/fakes/payment-sim" && "$GO" build -o "$RUN_DIR/payment-sim" . )
( cd "$ROOT/services/fakes/map-sim" && "$GO" build -o "$RUN_DIR/map-sim" . )
( cd "$ROOT/services/fakes/notify-sink" && "$GO" build -o "$RUN_DIR/notify-sink" . )

# Start placeholder, then gateway pointed at it.
PORT=8081 SERVICE_NAME=placeholder "$RUN_DIR/placeholder" \
  > "$RUN_DIR/placeholder.log" 2>&1 &
echo $! > "$RUN_DIR/placeholder.pid"

PORT=8080 PLACEHOLDER_URL="$PLACEHOLDER_URL" "$RUN_DIR/gateway" \
  > "$RUN_DIR/gateway.log" 2>&1 &
echo $! > "$RUN_DIR/gateway.pid"

# Start the fake providers (03 §5).
PORT=8091 PSP_SEED=42 PSP_TIMEOUT_MS=200 "$RUN_DIR/payment-sim" \
  > "$RUN_DIR/payment-sim.log" 2>&1 &
echo $! > "$RUN_DIR/payment-sim.pid"
PORT=8092 "$RUN_DIR/map-sim" > "$RUN_DIR/map-sim.log" 2>&1 &
echo $! > "$RUN_DIR/map-sim.pid"
PORT=8093 "$RUN_DIR/notify-sink" > "$RUN_DIR/notify-sink.log" 2>&1 &
echo $! > "$RUN_DIR/notify-sink.pid"

wait_healthy "$PLACEHOLDER_URL/healthz" placeholder
wait_healthy "$GATEWAY_URL/healthz" gateway
wait_healthy "$PAYMENT_SIM_URL/healthz" payment-sim
wait_healthy "$MAP_SIM_URL/healthz" map-sim
wait_healthy "$NOTIFY_SINK_URL/healthz" notify-sink
echo "stack up (process mode). logs in $RUN_DIR/*.log"
