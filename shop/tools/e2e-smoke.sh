#!/usr/bin/env bash
# e2e-smoke.sh — the S-T8 checkout->delivery smoke. Walks the ORDER LIFECYCLE
# (01 §4) across the WHOLE topology THROUGH THE GATEWAY, so it exercises routing +
# every slot regardless of whether that slot is a stub, a fake, or a real binary
# (mode-agnostic — that is the property the DoD verifies at all-stubs / one-real /
# all-real-but-one). Because most slots are contract stubs returning their example
# response, the smoke asserts what is meaningful at this layer:
#   - correct status codes + response shapes vs the contracts, hop by hop
#   - Idempotency-Replayed header on a repeat checkout POST (02 §3)
#   - payment-sim decline card (...0002 => 402) money-path branch (S-T7)
#   - notify-sink capture of the delivered-order notification (S-T7)
# Exits nonzero on the first failed assertion so `make e2e-smoke`, ci/run-local.sh
# and ci/post-merge-smoke.sh gate on it.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GW="${GATEWAY_URL:-http://localhost:8080}"
RUN="$ROOT/.run/e2e"
IDEM_KEY="idem_e2e_$(date +%s)_$$"
RECIPIENT="usr_e2e_$$"
fail=0
step=0

pass() { step=$((step + 1)); printf '  PASS [%02d] %s\n' "$step" "$1"; }
bad()  { step=$((step + 1)); printf '  FAIL [%02d] %s\n' "$step" "$1"; fail=$((fail + 1)); }

# assert_status <desc> <method> <path> <expect_code> [data]
assert_status() {
  local desc="$1" method="$2" path="$3" want="$4" data="${5:-}"
  local args=(-s -o /dev/null -w '%{http_code}' -X "$method" --max-time 8 "$GW$path")
  [ "$method" != GET ] && args+=(-H 'Content-Type: application/json' -H "Idempotency-Key: $IDEM_KEY")
  [ -n "$data" ] && args+=(-d "$data")
  local code; code="$(curl "${args[@]}" 2>/dev/null)"
  if [ "$code" = "$want" ]; then pass "$desc ($method $path -> $code)"; else bad "$desc ($method $path -> $code, want $want)"; fi
}

# assert_body <desc> <method> <path> <needle> [data]
assert_body() {
  local desc="$1" method="$2" path="$3" needle="$4" data="${5:-}"
  local args=(-s --max-time 8 -X "$method" "$GW$path")
  [ "$method" != GET ] && args+=(-H 'Content-Type: application/json' -H "Idempotency-Key: $IDEM_KEY")
  [ -n "$data" ] && args+=(-d "$data")
  local body; body="$(curl "${args[@]}" 2>/dev/null)"
  if [[ "$body" == *"$needle"* ]]; then pass "$desc (has $needle)"; else bad "$desc (missing '$needle' in: ${body:0:160})"; fi
}

echo "e2e-smoke: gateway=$GW"

# --- 0. topology present: every slot healthy THROUGH the gateway ---
echo "== topology health sweep (via gateway) =="
if [ -f "$RUN/plan.tsv" ]; then
  present=0; healthy=0
  while IFS=$'\t' read -r name port mode contract real_cmd; do
    [ -z "$name" ] && continue
    present=$((present + 1))
    if curl -fsS --max-time 3 "$GW/$name/healthz" >/dev/null 2>&1; then healthy=$((healthy + 1)); fi
  done < "$RUN/plan.tsv"
  if [ "$healthy" = "$present" ] && [ "$present" -ge 16 ]; then
    pass "all $present catalog+bff+fake slots healthy via gateway (>=16 required)"
  else
    bad "slot health: $healthy/$present healthy (need all, >=16)"
  fi
else
  bad "no .run/e2e/plan.tsv — is the env up? (make e2e-up)"
fi

# --- 1. quote (pricing-promo) ---
echo "== order lifecycle (01 §4) =="
QUOTE='{"cart_id":"crt_e2e","voucher_code":"LUNCH25","delivery_location":{"lat":13.7563,"lng":100.5018}}'
assert_status "quote created"            POST /pricing-promo/v1/quotes 201 "$QUOTE"
assert_body   "quote has quote_id"       POST /pricing-promo/v1/quotes 'quote_id' "$QUOTE"

# --- 2. checkout (order) — PAYMENT_PENDING, requires Idempotency-Key ---
CHECKOUT='{"quote_id":"qot_e2e","payment_method_id":"pm_e2e"}'
assert_status "checkout created"         POST /order/v1/orders 201 "$CHECKOUT"
assert_body   "checkout PAYMENT_PENDING" POST /order/v1/orders 'PAYMENT_PENDING' "$CHECKOUT"
ORDER_ID="$(curl -s --max-time 8 -X POST "$GW/order/v1/orders" -H 'Content-Type: application/json' -H "Idempotency-Key: $IDEM_KEY" -d "$CHECKOUT" 2>/dev/null | sed -n 's/.*"order_id":"\([^"]*\)".*/\1/p')"
ORDER_ID="${ORDER_ID:-ord_01H8XGJ2Q7Z9BQ3M4N5P6R7S8T}"

# --- 3. idempotency replay header on a REPEAT checkout with the same key ---
replay_hdr="$(curl -s -D - -o /dev/null --max-time 8 -X POST "$GW/order/v1/orders" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $IDEM_KEY" -d "$CHECKOUT" 2>/dev/null \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="idempotency-replayed"{print $2}')"
if [ "$replay_hdr" = "true" ]; then pass "repeat checkout replays (Idempotency-Replayed: true)"; else bad "repeat checkout missing Idempotency-Replayed header (got '$replay_hdr')"; fi

# --- 4. payment authorize (payment-sim fake) — success card ---
AUTH_OK='{"card_number":"4111111111111111","amount":{"amount":42550,"currency":"THB"},"order_ref":"'"$ORDER_ID"'"}'
assert_status "payment authorize (good card)" POST /payment-sim/v1/psp/authorize 200 "$AUTH_OK"
assert_body   "payment AUTHORIZED"            POST /payment-sim/v1/psp/authorize 'AUTHORIZED' "$AUTH_OK"

# --- 5. payment DECLINE branch (money path): card ...0002 => 402 ---
AUTH_NO='{"card_number":"4000000000000002","amount":{"amount":42550,"currency":"THB"}}'
assert_status "decline card ...0002 => 402"   POST /payment-sim/v1/psp/authorize 402 "$AUTH_NO"
assert_body   "decline envelope PSP_CARD_DECLINED" POST /payment-sim/v1/psp/authorize 'PSP_CARD_DECLINED' "$AUTH_NO"

# --- 6. merchant accept (merchant-bff) — PAID -> ACCEPTED ---
assert_status "merchant accept"          POST "/merchant-bff/v1/merchant/orders/$ORDER_ID:accept" 200 '{}'
assert_body   "accepted status ACCEPTED" POST "/merchant-bff/v1/merchant/orders/$ORDER_ID:accept" 'ACCEPTED' '{}'

# --- 7. dispatch assign (dispatch) — ACCEPTED -> DISPATCHED/ASSIGNED ---
assert_status "dispatch assign"          POST /dispatch/v1/assignments 201 '{"order_id":"'"$ORDER_ID"'"}'
assert_body   "assignment ASSIGNED"      POST /dispatch/v1/assignments 'ASSIGNED' '{"order_id":"'"$ORDER_ID"'"}'

# --- 8. pickup + delivery (driver-bff) ---
assert_status "driver pickup"            POST "/driver-bff/v1/driver/orders/$ORDER_ID:pickup" 200 '{}'
assert_body   "picked up"                POST "/driver-bff/v1/driver/orders/$ORDER_ID:pickup" 'PICKED_UP' '{}'
assert_status "driver deliver"           POST "/driver-bff/v1/driver/orders/$ORDER_ID:deliver" 200 '{}'
assert_body   "delivered"                POST "/driver-bff/v1/driver/orders/$ORDER_ID:deliver" 'DELIVERED' '{}'

# --- 9. order detail read shape (order GET) ---
assert_body   "order detail shape"       GET "/order/v1/orders/$ORDER_ID" 'order_id'

# --- 10. notification present in notify-sink inbox (delivered notification) ---
curl -s -X DELETE "$GW/notify-sink/v1/inbox?recipient=$RECIPIENT" >/dev/null 2>&1 || true
NOTE='{"channel":"PUSH","recipient":"'"$RECIPIENT"'","template":"order_delivered","subject":"Your order arrived"}'
assert_body   "notify-sink captured push" POST /notify-sink/v1/send 'message_id' "$NOTE"
assert_body   "notify-sink inbox has it"  GET "/notify-sink/v1/inbox?recipient=$RECIPIENT" '"count":1'

# --- 11. V-T1 edge auth (D4) — GATED on the identity slot being REAL so the
# all-stubs smoke stays green (the identity stub can't issue/verify tokens). ---
echo "== V-T1 edge auth (register->login->authed->forged->revoke) =="
IDENTITY_MODE="$(awk -F'\t' '$1=="identity"{print $3}' "$RUN/plan.tsv" 2>/dev/null)"
if [ "$IDENTITY_MODE" != "real" ]; then
  echo "  SKIP: identity slot mode='$IDENTITY_MODE' (not real) — auth section runs only when identity-auth is the real slot"
else
  AEMAIL="diner_e2e_$$@example.com"
  APW="hunter2pass"
  CREDS='{"email":"'"$AEMAIL"'","password":"'"$APW"'"}'
  # Register + login THROUGH the BFF passthrough (gateway routes /customer-bff/v1/auth/* to identity-auth).
  assert_status "auth register via customer-bff" POST /customer-bff/v1/auth/register 201 "$CREDS"
  LOGIN="$(curl -s --max-time 8 -X POST "$GW/customer-bff/v1/auth/login" -H 'Content-Type: application/json' -d "$CREDS" 2>/dev/null)"
  ACCESS="$(printf '%s' "$LOGIN" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
  REFRESH="$(printf '%s' "$LOGIN" | sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p')"
  if [ -n "$ACCESS" ] && [ -n "$REFRESH" ]; then pass "login via customer-bff issued access+refresh tokens"; else bad "login missing tokens (${LOGIN:0:160})"; fi

  # Authed request verified LOCALLY at the gateway (no call to identity on the hot path) -> 200.
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H "Authorization: Bearer $ACCESS" "$GW/order/healthz" 2>/dev/null)"
  if [ "$code" = 200 ]; then pass "authed request verified at gateway ($code)"; else bad "authed request -> $code want 200"; fi

  # Forged (bad-signature) token rejected 401 with the AUTH_TOKEN_INVALID envelope.
  FORGED="eyJhbGciOiJFUzI1NiIsImtpZCI6ImtfZm9yZ2VkIn0.eyJzdWIiOiJ1c3JfZXZpbCJ9.Zm9yZ2Vk"
  fbody="$(curl -s --max-time 8 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $FORGED" "$GW/order/healthz" 2>/dev/null)"
  if [ "$fbody" = 401 ]; then pass "forged token rejected at gateway (401)"; else bad "forged token -> $fbody want 401"; fi

  # Refresh rotates the refresh token.
  assert_status "refresh rotates via customer-bff" POST /customer-bff/v1/auth/refresh 200 '{"refresh_token":"'"$REFRESH"'"}'

  # Revocation propagates to the edge within the poll window (<=30s SLO; 5s poll here).
  L2="$(curl -s --max-time 8 -X POST "$GW/customer-bff/v1/auth/login" -H 'Content-Type: application/json' -d "$CREDS" 2>/dev/null)"
  A2="$(printf '%s' "$L2" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
  R2="$(printf '%s' "$L2" | sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p')"
  pre="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H "Authorization: Bearer $A2" "$GW/order/healthz" 2>/dev/null)"
  if [ "$pre" = 200 ]; then pass "fresh session passes before revoke ($pre)"; else bad "fresh session -> $pre want 200"; fi
  curl -s -o /dev/null --max-time 8 -X POST "$GW/identity/v1/auth/revoke" -H 'Content-Type: application/json' -d '{"refresh_token":"'"$R2"'"}' 2>/dev/null
  t0="$(date +%s)"; revoked=0; lag=0
  for _ in $(seq 1 32); do
    c="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H "Authorization: Bearer $A2" "$GW/order/healthz" 2>/dev/null)"
    if [ "$c" = 401 ]; then revoked=1; lag=$(( $(date +%s) - t0 )); break; fi
    sleep 1
  done
  if [ "$revoked" = 1 ]; then pass "revoked token rejected in ${lag}s (<=30s SLO, 5s poll)"; else bad "revoked token still accepted after 30s"; fi
fi

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "e2e-smoke: GREEN — $step/$step assertions passed (checkout->delivery across the full topology)"
  exit 0
else
  echo "e2e-smoke: RED — $fail of $step assertions failed"
  exit 1
fi
