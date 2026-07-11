#!/usr/bin/env bash
# e2e-realcmd.sh — the documented "real implementation" launcher used by the
# shared E2E env (S-T8) IN THIS REPO ONLY.
#
# The DoD needs the smoke proven mode-agnostic at one-real and all-real-but-one
# mixes, but no V-slice service binary exists in this repo yet (the slices are
# V-T1..V-T37, not built here). So a slot flipped to mode=real launches THIS
# script as its real_cmd: it boots a contract-shaped server for the slot on the
# same port the stub used, via the same stubgen the platform ships. This is the
# honest simulation the task authorises ("mark real_cmd = stub-binary alias").
#
# In production, real_cmd points at the actual merged service binary instead;
# e2e-up's launch path (PORT/CONTRACT/SERVICE_NAME in env) is identical, so the
# swap machinery under test here is exactly the machinery used for a real merge.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
CONTRACT="${CONTRACT:?CONTRACT env required}"
STUBGEN="${STUBGEN:-$ROOT/.run/e2e/bin/stubgen}"

[ -x "$STUBGEN" ] || ( cd "$ROOT/tools/stubgen" && "$GO" build -o "$STUBGEN" . )
exec "$STUBGEN" -spec "$ROOT/$CONTRACT" -port "$PORT" -idempotency
