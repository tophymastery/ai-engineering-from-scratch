#!/usr/bin/env bash
# identity-realcmd.sh — the real_cmd launcher for the identity slot in the shared
# E2E env (S-T8 swap machinery), used by V-T1. Unlike tools/e2e-realcmd.sh (which
# boots a contract STUB for slices with no binary), this boots the ACTUAL merged
# identity-auth service — the first real slice in the topology.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached after the first build) and execs it, so the
# swap path is identical to a production merge (binary reads PORT).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/identity-auth"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/identity-auth" && "$GO" build -o "$BIN" . )
fi

# ENV=e2e keeps the ops key-rotation endpoints available (non-prod) so the
# rotate-keys demo can run against the shared env. In-memory DB by default.
exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-identity-auth}" ENV="${ENV:-e2e}" \
  IDENTITY_DB="${IDENTITY_DB:-:memory:}" "$BIN"
