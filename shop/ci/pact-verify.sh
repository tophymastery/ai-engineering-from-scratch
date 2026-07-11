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

# --- V-T3: search/cart -> merchant-catalog pacts, verified against the REAL service ---
CPORT="${CATALOG_PROVIDER_PORT:-18102}"
CBASE="http://localhost:$CPORT"
C_PID=""
c_cleanup() { [ -n "$C_PID" ] && kill "$C_PID" 2>/dev/null || true; }
trap 'cleanup; id_cleanup; p_cleanup; c_cleanup' EXIT

sub "build + boot merchant-catalog provider (V-T3) on $CBASE (catalog_v1 on)"
( cd services/merchant-catalog && "$GO" build -o "$BIN/merchant-catalog" . )
PORT="$CPORT" SERVICE_NAME=merchant-catalog ENV=dev FLAG_CATALOG_V1=true "$BIN/merchant-catalog" >"$BIN/catalog.log" 2>&1 &
C_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 "$CBASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS --max-time 2 "$CBASE/healthz" >/dev/null || { echo "merchant-catalog never healthy"; cat "$BIN/catalog.log"; exit 1; }
echo "merchant-catalog healthy"

# Provider state (like the identity-auth login seed): a fixed merchant with one
# menu item + OPEN store, so the search/cart read interactions have something to
# read. The ETag is taken from the GET response header — exactly the If-Match value.
CMID="mer_01hpactcatalog0000000000000"
curl -fsS -X POST "$CBASE/v1/merchants" -H 'Content-Type: application/json' \
  -d '{"merchant_id":"'"$CMID"'","name":"Pact Kitchen"}' >/dev/null \
  || { echo "failed to seed pact merchant"; exit 1; }
MENU_ETAG="$(curl -s -D - -o /dev/null "$CBASE/v1/merchants/$CMID/menu" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
curl -fsS -X PATCH "$CBASE/v1/merchants/$CMID/menu" -H 'Content-Type: application/json' -H "If-Match: $MENU_ETAG" \
  -d '{"upsert_items":[{"name":"Som Tam","price":{"amount":8000,"currency":"THB"},"available":true}]}' >/dev/null \
  || { echo "failed to seed pact menu item"; exit 1; }
STATUS_ETAG="$(curl -s -D - -o /dev/null "$CBASE/v1/merchants/$CMID/store-status" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
curl -fsS -X PUT "$CBASE/v1/merchants/$CMID/store-status" -H 'Content-Type: application/json' -H "If-Match: $STATUS_ETAG" \
  -d '{"status":"OPEN"}' >/dev/null || { echo "failed to seed pact store status"; exit 1; }

sub "verify search -> merchant-catalog pact (menu + store-status reads) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/search__merchant-catalog.json" "$CBASE"
sub "verify cart -> merchant-catalog pact (item price + availability read) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/cart__merchant-catalog.json" "$CBASE"

# --- V-T4: customer-bff -> search (browse feed + geo search), verified against
# the REAL search-query service (search_v2 on) ---
SPORT="${SEARCH_PROVIDER_PORT:-18103}"
SBASE="http://localhost:$SPORT"
S_PID=""
s_cleanup() { [ -n "$S_PID" ] && kill "$S_PID" 2>/dev/null || true; }
trap 'cleanup; id_cleanup; p_cleanup; c_cleanup; s_cleanup' EXIT

sub "build + boot search-query provider (V-T4) on $SBASE (search_v2 on)"
( cd services/search-query && "$GO" build -o "$BIN/search-query" . )
PORT="$SPORT" SERVICE_NAME=search-query ENV=dev FLAG_SEARCH_V2=true "$BIN/search-query" >"$BIN/search.log" 2>&1 &
S_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 "$SBASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS --max-time 2 "$SBASE/healthz" >/dev/null || { echo "search-query never healthy"; cat "$BIN/search.log"; exit 1; }
echo "search-query healthy"

# Provider state: seed the fixed store at the pact's query point.
curl -fsS -X POST "$SBASE/v1/index/merchants" -H 'Content-Type: application/json' \
  -d '{"merchant_id":"mer_01hpactsearch000000000000","name":"Pact Som Tam","lat":13.7563,"lng":100.5018,"open":true,"rating":4.7,"menu_version":1,"items":[{"item_id":"itm_p","name":"Som Tam","amount":8000,"currency":"THB","available":true}]}' >/dev/null \
  || { echo "failed to seed pact search doc"; exit 1; }

sub "verify customer-bff -> search pact (browse feed + geo search) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/customer-bff__search.json" "$SBASE"

# --- V-T5: customer-bff -> ranking (re-rank contract), verified against the REAL
# ranking service (ranking_ml on). The /v1/rank interaction is self-contained (no
# search retrieval), so no SEARCH_URL wiring is needed here. ---
RKPORT="${RANKING_PROVIDER_PORT:-18115}"
RKBASE="http://localhost:$RKPORT"
RK_PID=""
rk_cleanup() { [ -n "$RK_PID" ] && kill "$RK_PID" 2>/dev/null || true; }
trap 'cleanup; id_cleanup; p_cleanup; c_cleanup; s_cleanup; rk_cleanup' EXIT

sub "build + boot ranking provider (V-T5) on $RKBASE (ranking_ml on)"
( cd services/ranking && "$GO" build -o "$BIN/ranking" . )
PORT="$RKPORT" SERVICE_NAME=ranking ENV=dev FLAG_RANKING_ML=true "$BIN/ranking" >"$BIN/ranking.log" 2>&1 &
RK_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 "$RKBASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS --max-time 2 "$RKBASE/healthz" >/dev/null || { echo "ranking never healthy"; cat "$BIN/ranking.log"; exit 1; }
echo "ranking healthy"

sub "verify customer-bff -> ranking pact (re-rank top-K) — expect GREEN"
"$REG" pact-verify "$ROOT/contracts/pacts/customer-bff__ranking.json" "$RKBASE"

echo
echo "pact-verify: GREEN — placeholder + identity-auth + identity-profile + merchant-catalog + search + ranking honour their pacts; broken pact correctly reds the build"
