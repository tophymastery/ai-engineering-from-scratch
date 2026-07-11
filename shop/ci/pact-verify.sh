#!/usr/bin/env bash
# pact-verify.sh — the S-T5 PACT-VERIFY merge gate (file-based Pact broker).
#
# pact-broker binaries are unavailable in this environment, so the broker is
# file-based: contracts/pacts/<consumer>__<provider>.json are Pact-v2 shaped and
# registryctl pact-verify REPLAYS each interaction against the ACTUALLY-RUNNING
# provider, asserting response status + shape. This gate is what makes "breaking
# a published pact => provider build red" real.
#
# Steps:
#   1. build + boot the placeholder provider (S-T1/S-T3), wait healthy
#   2. verify the seed pact customer-bff -> placeholder   (expect GREEN)
#   3. verify the broken fixture (provider missing an interaction) — expected-fail
#      must exit nonzero (like the S-T2 backdoor fixture)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PACT_PROVIDER_PORT:-18099}"
BASE="http://localhost:$PORT"
BIN="$(mktemp -d)"
PROV_PID=""
cleanup() { [ -n "$PROV_PID" ] && kill "$PROV_PID" 2>/dev/null || true; rm -rf "$BIN"; }
trap cleanup EXIT

sub() { echo; echo "-- $* --"; }

sub "build registryctl + placeholder provider"
( cd contracts/registryctl && "$GO" build -o "$BIN/registryctl" . )
( cd services/_placeholder && "$GO" build -o "$BIN/placeholder" . )
REG="$BIN/registryctl"

sub "boot provider (placeholder) on $BASE"
PORT="$PORT" SERVICE_NAME=placeholder "$BIN/placeholder" >"$BIN/provider.log" 2>&1 &
PROV_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS --max-time 2 "$BASE/healthz" >/dev/null || { echo "provider never healthy"; cat "$BIN/provider.log"; exit 1; }
echo "provider healthy"

sub "verify seed pact (customer-bff -> placeholder) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/customer-bff__placeholder.json" "$BASE"

sub "verify BROKEN-pact fixture (expected-fail — must exit nonzero)"
if "$REG" pact-verify "$ROOT/contracts/fixtures/pact-red/customer-bff__placeholder.broken.json" "$BASE" >/dev/null 2>&1; then
  echo "ERROR: broken-pact fixture should have failed the provider verification"; exit 1
else
  echo "broken-pact fixture correctly failed (published-pact break => provider red, proven)"
fi

echo
echo "pact-verify: GREEN — provider honours the published pact; broken pact correctly reds the build"
