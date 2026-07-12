#!/usr/bin/env bash
# location-tracking-realcmd.sh — the real_cmd launcher for the `location-tracking`
# slot in the shared E2E env (S-T8 swap machinery), used by V-T13. Like
# tools/dispatch-realcmd.sh it boots the ACTUAL merged service (the
# `location-gateway` binary, not a contract stub), so the auth-once + 100 ms batch
# ingest, the H3 res-7 geo index, the kNN read dispatch consumes, and the 1:10 →
# Iceberg / PG-trip-summary tiering run against the real telemetry plane in the
# shared topology.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It builds
# the binary on demand (cached) and execs it. telemetry_v2 is forced ON here
# (FLAG_TELEMETRY_V2=true) so the E2E env demos the slice "with the flag on".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/location-gateway"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/location-gateway" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-location-gateway}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_TELEMETRY_V2="${FLAG_TELEMETRY_V2:-true}" \
  BATCH_WINDOW="${BATCH_WINDOW:-100ms}" GEO_TTL="${GEO_TTL:-30s}" \
  TELEMETRY_PARTITIONS="${TELEMETRY_PARTITIONS:-64}" LOG_SAMPLE_RATE="${LOG_SAMPLE_RATE:-0.02}" "$BIN"
