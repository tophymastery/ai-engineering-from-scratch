#!/usr/bin/env bash
# identity-profile-realcmd.sh — the real_cmd launcher for the identity-profile
# slot in the shared E2E env (S-T8 swap machinery), used by V-T2. Like
# tools/identity-realcmd.sh it boots the ACTUAL merged service (not a contract
# stub), so the profile CRUD + erasure demo runs against the real PII stores.
#
# e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in the environment. It
# builds the binary on demand (cached) and execs it, so the swap path is
# identical to a production merge (binary reads PORT).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/identity-profile"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/identity-profile" && "$GO" build -o "$BIN" . )
fi

# ENV=e2e; per-process random KEK; in-memory per-jurisdiction stores. The service
# is homed for the residency jurisdictions plus common cells.
exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-identity-profile}" ENV="${ENV:-e2e}" \
  PROFILE_JURISDICTIONS="${PROFILE_JURISDICTIONS:-ID,VN,SG,TH,MY,PH}" "$BIN"
