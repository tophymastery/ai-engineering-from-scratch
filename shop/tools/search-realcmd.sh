#!/usr/bin/env bash
# search-realcmd.sh — the real_cmd launcher for the `search` slot in the shared
# E2E env (S-T8 swap machinery), used by V-T4. It boots the ACTUAL search-query
# service (not a contract stub) so the browse feed (GET /v1/customer/home) + geo
# search demo runs against the real in-process index (H3-res-5 routing, salting,
# rating debounce, backpressure — services/search-indexer/index).
#
# In production search-query and search-indexer are two deployments over a shared
# per-cell OpenSearch; this sandbox has no OpenSearch and no cross-process shared
# store, so the E2E `search` slot runs search-query, which EMBEDS the indexer
# (index.Node) and is fed via its /v1/index/* ingest endpoints (disclosed in
# VERIFICATION.md §V-T4). e2e-up invokes this with PORT/CONTRACT/SERVICE_NAME in
# the environment. search_v2 is forced ON here so the E2E env demos "flag on".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:?PORT env required}"
BIN="$ROOT/.run/e2e/bin/search-query"

if [ ! -x "$BIN" ]; then
  mkdir -p "$(dirname "$BIN")"
  ( cd "$ROOT/services/search-query" && "$GO" build -o "$BIN" . )
fi

exec env PORT="$PORT" SERVICE_NAME="${SERVICE_NAME:-search-query}" ENV="${ENV:-e2e}" \
  REGION="${REGION:-bkk}" FLAG_SEARCH_V2="${FLAG_SEARCH_V2:-true}" "$BIN"
