#!/usr/bin/env bash
# rotate-keys-demo.sh — REHEARSES docs/runbooks/key-rotation.md end-to-end
# against the real identity-auth service and a real gateway edge (V-T1 / D4).
#
# The invariant being proven (also asserted in the Go unit test
# services/identity-auth TestKeyRotationRunbook):
#   1. add key B  -> JWKS advertises BOTH A and B; new tokens are signed with B
#   2. OVERLAP    -> tokens signed by A STILL verify at the edge (no forced logout)
#   3. retire A   -> JWKS drops A; a FRESH edge rejects A-signed tokens, keeps B
#
# Boots two gateways: the "live" one that cached A during the overlap (A stays
# valid there until its tokens expire — exactly the runbook's reason to retire A
# only after ≤15 min), and a "fresh" one started AFTER retirement to prove a
# newly-rolled edge no longer honours A. Exits nonzero on any failed assertion.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
BIN="$(mktemp -d)"
IDPORT=18201
GW1=18280   # live edge (caches A during overlap)
GW2=18281   # fresh edge (started after retire)
PIDS=()
fail=0
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$BIN"; }
trap cleanup EXIT

step=0
ok()  { step=$((step+1)); printf '  PASS [%02d] %s\n' "$step" "$1"; }
no()  { step=$((step+1)); printf '  FAIL [%02d] %s\n' "$step" "$1"; fail=$((fail+1)); }

jval() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p'; }
kidcount() { grep -o '"kid"' | wc -l | tr -d ' '; }
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$@" 2>/dev/null; }

echo "== build identity-auth + gateway =="
( cd "$ROOT/services/identity-auth" && "$GO" build -o "$BIN/identity-auth" . )
( cd "$ROOT/gateway" && "$GO" build -o "$BIN/gateway" . )

# routes: one prefix -> identity-auth (gateway discovers the JWKS/denylist source
# from the /identity/ route).
printf '[{"prefix":"/identity/","upstream":"http://localhost:%s"}]\n' "$IDPORT" > "$BIN/routes.json"

echo "== boot identity-auth (:$IDPORT) + live gateway (:$GW1) =="
PORT="$IDPORT" SERVICE_NAME=identity-auth ENV=e2e "$BIN/identity-auth" >"$BIN/id.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 40); do curl -fsS --max-time 1 "http://localhost:$IDPORT/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
PORT="$GW1" GATEWAY_ROUTES="$BIN/routes.json" FLAG_AUTH_JWT_EDGE=true DENYLIST_POLL=1s "$BIN/gateway" >"$BIN/gw1.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 40); do curl -fsS --max-time 1 "http://localhost:$GW1/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

CREDS='{"email":"rotate@example.com","password":"hunter2pass"}'
curl -fsS -X POST "http://localhost:$IDPORT/v1/auth/register" -H 'Content-Type: application/json' -d "$CREDS" >/dev/null

# --- token under key A ---
LA="$(curl -s -X POST "http://localhost:$IDPORT/v1/auth/login" -H 'Content-Type: application/json' -d "$CREDS")"
TOKA="$(printf '%s' "$LA" | jval access_token)"; KIDA="$(printf '%s' "$LA" | jval kid)"
[ -n "$TOKA" ] && ok "issued token under key A (kid=$KIDA)" || no "no token A"
n="$(curl -s "http://localhost:$IDPORT/.well-known/jwks.json" | kidcount)"
[ "$n" = 1 ] && ok "JWKS advertises 1 key before rotation" || no "JWKS key count=$n want 1"
c="$(code -H "Authorization: Bearer $TOKA" "http://localhost:$GW1/identity/healthz")"
[ "$c" = 200 ] && ok "token A verifies at the live edge ($c)" || no "token A at edge -> $c"

# --- STEP 1: rotate (add key B, sign new tokens with B) ---
R="$(curl -s -X POST "http://localhost:$IDPORT/v1/auth/keys:rotate" -H 'Content-Type: application/json' -d '{}')"
KIDB="$(printf '%s' "$R" | jval primary_kid)"
[ -n "$KIDB" ] && [ "$KIDB" != "$KIDA" ] && ok "rotated: new primary key B (kid=$KIDB)" || no "rotate did not add a distinct key"
n="$(curl -s "http://localhost:$IDPORT/.well-known/jwks.json" | kidcount)"
[ "$n" = 2 ] && ok "JWKS now advertises BOTH keys (A+B)" || no "JWKS key count=$n want 2"

LB="$(curl -s -X POST "http://localhost:$IDPORT/v1/auth/login" -H 'Content-Type: application/json' -d "$CREDS")"
TOKB="$(printf '%s' "$LB" | jval access_token)"; KIDBt="$(printf '%s' "$LB" | jval kid)"
[ "$KIDBt" = "$KIDB" ] && ok "new tokens are signed with key B" || no "new token kid=$KIDBt want $KIDB"

# --- STEP 2: OVERLAP — both A and B verify at the edge ---
# The live edge cached JWKS at startup (key A only); the first token carrying the
# new kid B triggers a throttled JWKS refresh, so poll briefly for pickup (≤1s).
c=""
for _ in $(seq 1 20); do
  c="$(code -H "Authorization: Bearer $TOKB" "http://localhost:$GW1/identity/healthz")"
  [ "$c" = 200 ] && break; sleep 0.25
done
[ "$c" = 200 ] && ok "token B verifies at the edge (JWKS refresh on new kid) ($c)" || no "token B at edge -> $c"
c="$(code -H "Authorization: Bearer $TOKA" "http://localhost:$GW1/identity/healthz")"
[ "$c" = 200 ] && ok "OVERLAP: old token A STILL verifies during rotation ($c)" || no "token A rejected during overlap -> $c"

# --- STEP 3: retire A (only safe once all A-signed tokens have expired) ---
RT="$(curl -s -X POST "http://localhost:$IDPORT/v1/auth/keys:retire" -H 'Content-Type: application/json' -d '{}')"
RETIRED="$(printf '%s' "$RT" | jval retired_kid)"
[ "$RETIRED" = "$KIDA" ] && ok "retired key A (kid=$KIDA)" || no "retire removed wrong kid ($RETIRED)"
n="$(curl -s "http://localhost:$IDPORT/.well-known/jwks.json" | kidcount)"
[ "$n" = 1 ] && ok "JWKS drops A — 1 key (B) remains" || no "JWKS key count=$n want 1 after retire"
if curl -s "http://localhost:$IDPORT/.well-known/jwks.json" | grep -q "$KIDA"; then no "kid A still present in JWKS after retire"; else ok "kid A absent from JWKS after retire"; fi

# A FRESH edge (rolled after retirement) has only key B: it rejects A, keeps B.
PORT="$GW2" GATEWAY_ROUTES="$BIN/routes.json" FLAG_AUTH_JWT_EDGE=true DENYLIST_POLL=1s "$BIN/gateway" >"$BIN/gw2.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 40); do curl -fsS --max-time 1 "http://localhost:$GW2/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
c="$(code -H "Authorization: Bearer $TOKA" "http://localhost:$GW2/identity/healthz")"
[ "$c" = 401 ] && ok "FRESH edge rejects retired key-A token ($c)" || no "fresh edge accepted A -> $c want 401"
c="$(code -H "Authorization: Bearer $TOKB" "http://localhost:$GW2/identity/healthz")"
[ "$c" = 200 ] && ok "FRESH edge still accepts key-B token ($c)" || no "fresh edge rejected B -> $c"

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "rotate-keys-demo: GREEN — $step/$step assertions; key-rotation runbook rehearsed (A→B overlap→retire A)"
  exit 0
else
  echo "rotate-keys-demo: RED — $fail of $step assertions failed"; exit 1
fi
