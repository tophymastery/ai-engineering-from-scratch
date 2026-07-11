#!/usr/bin/env bash
# e2e-up.sh — boot the WHOLE shared E2E topology (S-T8) in process mode from the
# single-source manifest deploy/e2e/topology.yaml + the runtime overlay
# .run/e2e-overlay.yaml (stub->real swaps). Every catalog service + BFF becomes a
# stubgen instance (or its S-T7 fake, or a real binary), and the gateway fans out
# one prefix per slot. Healthchecks everything, then prints the topology summary.
#
# "Automatic from manifests": this script re-reads manifest+overlay on EVERY
# invocation, so `make e2e-sync` (which writes real_cmd swaps into the overlay)
# followed by e2e-up brings merged implementations live with no manifest edit.
#
# No Docker required (mirrors tools/dev-up.sh process fallback): std-lib Go
# binaries + curl only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
RUN="$ROOT/.run/e2e"
BIN="$RUN/bin"
LOG="$RUN/log"
TOPO="$ROOT/deploy/e2e/topology.yaml"
OVERLAY="$ROOT/.run/e2e-overlay.yaml"
GATEWAY_PORT="${GATEWAY_PORT:-8080}"

# Tear down any prior run first so a re-invoke is a clean re-read.
"$ROOT/tools/e2e-down.sh" >/dev/null 2>&1 || true

mkdir -p "$BIN" "$LOG"

echo "== building topology binaries =="
( cd "$ROOT/tools/e2ectl"  && "$GO" build -o "$BIN/e2ectl"  . )
( cd "$ROOT/tools/stubgen" && "$GO" build -o "$BIN/stubgen" . )
( cd "$ROOT/gateway"       && "$GO" build -o "$BIN/gateway" . )
( cd "$ROOT/services/_placeholder" && "$GO" build -o "$BIN/placeholder-real" . )
for f in payment-sim map-sim notify-sink; do
  ( cd "$ROOT/services/fakes/$f" && "$GO" build -o "$BIN/$f" . )
done
# V-T1: prebuild the identity-auth slice binary so the identity slot's real_cmd
# (tools/identity-realcmd.sh) execs it immediately when swapped to real.
( cd "$ROOT/services/identity-auth" && "$GO" build -o "$BIN/identity-auth" . )
E2ECTL="$BIN/e2ectl"
export STUBGEN="$BIN/stubgen"

wait_healthy() {
  local url="$1" name="$2" tries="${3:-60}"
  for _ in $(seq 1 "$tries"); do
    curl -fsS --max-time 2 "$url" >/dev/null 2>&1 && { echo "  healthy: $name ($url)"; return 0; }
    sleep 0.25
  done
  echo "  ERROR: $name never healthy at $url" >&2
  echo "  --- last log lines ($name) ---" >&2
  tail -n 8 "$LOG/$name.log" >&2 2>/dev/null || true
  return 1
}

# Resolve the plan (manifest + overlay) and launch each slot per its mode.
echo "== launching slots (manifest + overlay) =="
: > "$RUN/plan.tsv"
n=0
while IFS=$'\t' read -r name port mode contract real_cmd; do
  [ -z "$name" ] && continue
  echo "$name	$port	$mode	$contract	$real_cmd" >> "$RUN/plan.tsv"
  case "$mode" in
    stub)
      "$STUBGEN" -spec "$ROOT/$contract" -port "$port" -idempotency \
        > "$LOG/$name.log" 2>&1 &
      ;;
    fake)
      PORT="$port" "$BIN/$name" > "$LOG/$name.log" 2>&1 &
      ;;
    real)
      [ -n "$real_cmd" ] || { echo "ERROR: $name mode=real but empty real_cmd" >&2; exit 1; }
      PORT="$port" CONTRACT="$contract" SERVICE_NAME="$name" \
        bash -c "$real_cmd" > "$LOG/$name.log" 2>&1 &
      ;;
    *)
      echo "ERROR: $name has unknown mode $mode" >&2; exit 1;;
  esac
  echo $! > "$RUN/$name.pid"
  n=$((n + 1))
done < <("$E2ECTL" plan "$TOPO" "$OVERLAY")

# Gateway: routes from the SAME resolved plan, one prefix per slot.
# V-T1/D4: auth_jwt_edge defaults ON in the shared E2E env (the prod overlay ships
# it OFF until rollout). With no bearer token presented the verifier is a no-op, so
# all-stubs smoke stays green; when a token IS presented the gateway verifies it
# locally against identity-auth's JWKS + polled denylist (DENYLIST_POLL, 5s here).
"$E2ECTL" routes "$TOPO" "$OVERLAY" > "$RUN/routes.json"
PORT="$GATEWAY_PORT" GATEWAY_ROUTES="$RUN/routes.json" GATEWAY_MODE="${GATEWAY_MODE:-dev}" \
  FLAG_AUTH_JWT_EDGE="${FLAG_AUTH_JWT_EDGE:-true}" DENYLIST_POLL="${DENYLIST_POLL:-5s}" \
  "$BIN/gateway" > "$LOG/gateway.log" 2>&1 &
echo $! > "$RUN/gateway.pid"

echo "== healthchecking $n slot(s) + gateway =="
while IFS=$'\t' read -r name port mode contract real_cmd; do
  [ -z "$name" ] && continue
  wait_healthy "http://localhost:$port/healthz" "$name"
done < "$RUN/plan.tsv"
wait_healthy "http://localhost:$GATEWAY_PORT/healthz" gateway

# Prove the gateway actually routes to a slot end-to-end (not just its own /healthz).
if curl -fsS --max-time 2 "http://localhost:$GATEWAY_PORT/order/healthz" | grep -q '"service":"order"'; then
  echo "  gateway routing verified: /order/* -> order slot"
else
  echo "  ERROR: gateway did not route /order/* to the order slot" >&2; exit 1
fi

echo "process" > "$RUN/mode"
stubs=$(awk -F'\t' '$3=="stub"' "$RUN/plan.tsv" | wc -l | tr -d ' ')
fakes=$(awk -F'\t' '$3=="fake"' "$RUN/plan.tsv" | wc -l | tr -d ' ')
reals=$(awk -F'\t' '$3=="real"' "$RUN/plan.tsv" | wc -l | tr -d ' ')
echo "== E2E env UP: $n slots ($stubs stub, $fakes fake, $reals real) + gateway = $((n + 1)) processes, all healthy =="
echo "   manifest=deploy/e2e/topology.yaml overlay=$( [ -f "$OVERLAY" ] && echo "$OVERLAY" || echo "(none)" ) logs=$LOG"
