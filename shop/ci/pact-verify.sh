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

# --- V-T1: customer-bff -> identity-auth pact, verified against the REAL service ---
IDPORT="${IDENTITY_PROVIDER_PORT:-18101}"
IDBASE="http://localhost:$IDPORT"
ID_PID=""
id_cleanup() { [ -n "$ID_PID" ] && kill "$ID_PID" 2>/dev/null || true; }
trap 'cleanup; id_cleanup' EXIT

sub "build + boot identity-auth provider (V-T1) on $IDBASE"
( cd services/identity-auth && "$GO" build -o "$BIN/identity-auth" . )
PORT="$IDPORT" SERVICE_NAME=identity-auth ENV=dev "$BIN/identity-auth" >"$BIN/identity.log" 2>&1 &
ID_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 "$IDBASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS --max-time 2 "$IDBASE/healthz" >/dev/null || { echo "identity-auth never healthy"; cat "$BIN/identity.log"; exit 1; }
echo "identity-auth healthy"

# Provider state for the login interaction: pre-register the login user.
curl -fsS -X POST "$IDBASE/v1/auth/register" -H 'Content-Type: application/json' \
  -d '{"email":"pact-login@example.com","password":"hunter2pass"}' >/dev/null \
  || { echo "failed to seed pact login user"; exit 1; }

sub "verify customer-bff -> identity-auth pact (register + login) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/customer-bff__identity-auth.json" "$IDBASE"

# --- V-T2: customer-bff -> identity-profile pact, verified against the REAL service ---
PPORT="${PROFILE_PROVIDER_PORT:-18113}"
PBASE="http://localhost:$PPORT"
P_PID=""
p_cleanup() { [ -n "$P_PID" ] && kill "$P_PID" 2>/dev/null || true; }
trap 'cleanup; id_cleanup; p_cleanup' EXIT

sub "build + boot identity-profile provider (V-T2) on $PBASE"
( cd services/identity-profile && "$GO" build -o "$BIN/identity-profile" . )
PORT="$PPORT" SERVICE_NAME=identity-profile ENV=dev "$BIN/identity-profile" >"$BIN/profile.log" 2>&1 &
P_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 "$PBASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS --max-time 2 "$PBASE/healthz" >/dev/null || { echo "identity-profile never healthy"; cat "$BIN/profile.log"; exit 1; }
echo "identity-profile healthy"

sub "verify customer-bff -> identity-profile pact (create-profile + token-resolve) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/customer-bff__identity-profile.json" "$PBASE"

echo
echo "pact-verify: GREEN — placeholder + identity-auth + identity-profile honour their pacts; broken pact correctly reds the build"
