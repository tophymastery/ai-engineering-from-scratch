#!/usr/bin/env bash
# merchant-catalog-realcmd.sh — the real_cmd launcher for the merchant-catalog
# slot in the shared E2E env (S-T8 swap machinery), used by V-T3. Like
# tools/identity-profile-realcmd.sh it boots the ACTUAL merged service (not a
# contract stub), so the menu-editor + store-status demo (incl. the 412 stale
# write) runs against the real catalog store + transactional outbox.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it. catalog_v1 is forced ON here
# (FLAG_CATALOG_V1=true) so the E2E env demos the slice "with the flag on".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/merchant-catalog"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/merchant-catalog" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-merchant-catalog}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_CATALOG_V1="${FLAG_CATALOG_V1:-true}" "$BIN"
