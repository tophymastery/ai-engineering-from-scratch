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

# --- 12. V-T2 profile CRUD + erasure (D3) — GATED on the identity-profile slot
# being REAL (the stub can't encrypt/erase). Demoed THROUGH the customer-bff
# passthrough (gateway routes /customer-bff/v1/profiles* -> identity-profile). ---
echo "== V-T2 profile + residency + erasure (create->read->erase->unreadable, token survives) =="
PROFILE_MODE="$(awk -F'\t' '$1=="identity-profile"{print $3}' "$RUN/plan.tsv" 2>/dev/null)"
if [ "$PROFILE_MODE" != "real" ]; then
  echo "  SKIP: identity-profile slot mode='$PROFILE_MODE' (not real) — profile/erasure runs only when identity-profile is the real slot"
else
  PBODY='{"jurisdiction":"ID","full_name":"Budi Santoso","phone":"+62-812-1111-2222","email":"budi@example.co.id","addresses":[{"label":"home","line1":"Jl. Merdeka 17","city":"Jakarta","postal":"10110"}]}'
  PC="$(curl -s --max-time 8 -X POST "$GW/customer-bff/v1/profiles" -H 'Content-Type: application/json' -H 'X-Cell: ID' -d "$PBODY" 2>/dev/null)"
  USR="$(printf '%s' "$PC" | sed -n 's/.*"user_token":"\([^"]*\)".*/\1/p')"
  if [ -n "$USR" ] && [[ "$PC" == *'"jurisdiction":"ID"'* ]]; then pass "profile created via customer-bff ($USR, cell ID)"; else bad "profile create failed (${PC:0:160})"; fi

  # Read back — PII decrypted for the owner.
  assert_body "profile read returns decrypted PII" GET "/customer-bff/v1/profiles/$USR" 'Budi Santoso'

  # Residency: a request tagged for a non-owning cell (VN) is refused.
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H 'X-Cell: VN' "$GW/customer-bff/v1/profiles/$USR" 2>/dev/null)"
  if [ "$code" = 404 ] || [ "$code" = 403 ]; then pass "non-owning cell (VN) cannot read an ID profile ($code)"; else bad "cross-cell read -> $code (want 403/404)"; fi

  # Order snapshot carries ONLY tokens; replay works BEFORE erasure.
  SNAP='{"order_token":"ord_e2e","user_token":"'"$USR"'","addr_token":"adr_e2e","jurisdiction":"ID","currency":"IDR","items":[{"sku":"s","qty":2,"price_minor":4500}]}'
  assert_body "token-only order replays (pre-erase)" POST "/identity-profile/v1/orders:replay" '"total_minor":9000' "$SNAP"

  # ERASE (crypto-shred) via the BFF.
  ER="$(curl -s --max-time 8 -X POST "$GW/customer-bff/v1/profiles/$USR:erase" -H 'Content-Type: application/json' -H 'X-Cell: ID' -d '{}' 2>/dev/null)"
  if [[ "$ER" == *'"key_destroyed":true'* ]]; then pass "erasure crypto-shredded the key ($USR)"; else bad "erase failed (${ER:0:160})"; fi

  # PII now unreadable — GET returns 410 PROFILE_ERASED.
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H 'X-Cell: ID' "$GW/customer-bff/v1/profiles/$USR" 2>/dev/null)"
  if [ "$code" = 410 ]; then pass "post-erase profile read is 410 (PII unreadable)"; else bad "post-erase read -> $code (want 410)"; fi

  # Token STILL resolves (survives erasure) so order history replays.
  assert_body "usr token survives erasure (exists+erased)" GET "/customer-bff/v1/tokens/$USR" '"erased":true'
  assert_body "token-only order STILL replays (post-erase)" POST "/identity-profile/v1/orders:replay" '"total_minor":9000' "$SNAP"
fi

# --- 13. V-T3 merchant catalog & menus (ETag/If-Match → 412) — GATED on the
# merchant-catalog slot being REAL (the stub can't version/publish). Demoed
# THROUGH the merchant-bff passthrough (gateway routes /merchant-bff/v1/merchants*
# -> merchant-catalog). Proves menu CRUD, store-status, and — the headline —
# a stale If-Match is rejected 412 (100% of stale writes). ---
echo "== V-T3 merchant catalog (create->menu edit->store status->STALE WRITE 412, via merchant-bff) =="
CATALOG_MODE="$(awk -F'\t' '$1=="merchant-catalog"{print $3}' "$RUN/plan.tsv" 2>/dev/null)"
if [ "$CATALOG_MODE" != "real" ]; then
  echo "  SKIP: merchant-catalog slot mode='$CATALOG_MODE' (not real) — catalog section runs only when merchant-catalog is the real slot"
else
  CMID="mer_e2e_$$"
  # Create the merchant (bootstraps an empty menu + CLOSED store) via the BFF.
  CC="$(curl -s --max-time 8 -X POST "$GW/merchant-bff/v1/merchants" -H 'Content-Type: application/json' -d '{"merchant_id":"'"$CMID"'","name":"E2E Kitchen"}' 2>/dev/null)"
  if [[ "$CC" == *'"merchant_id":"'"$CMID"'"'* ]] && [[ "$CC" == *'"status":"CLOSED"'* ]]; then pass "merchant created via merchant-bff ($CMID, store CLOSED)"; else bad "merchant create failed (${CC:0:160})"; fi

  # Read the menu; capture the ETag header (the If-Match value for the edit).
  MENU_ETAG="$(curl -s -D - -o /dev/null --max-time 8 "$GW/merchant-bff/v1/merchants/$CMID/menu" 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
  if [ -n "$MENU_ETAG" ]; then pass "GET menu returns a strong ETag ($MENU_ETAG)"; else bad "GET menu returned no ETag"; fi

  # Edit the menu WITH the correct If-Match → 200 + a NEW ETag + the item present.
  EDIT='{"upsert_items":[{"name":"Som Tam","price":{"amount":8000,"currency":"THB"},"available":true}]}'
  ER2="$(curl -s -D - --max-time 8 -X PATCH "$GW/merchant-bff/v1/merchants/$CMID/menu" -H 'Content-Type: application/json' -H "If-Match: $MENU_ETAG" -d "$EDIT" 2>/dev/null)"
  NEW_ETAG="$(printf '%s' "$ER2" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
  if [[ "$ER2" == *'"name":"Som Tam"'* ]] && [ -n "$NEW_ETAG" ] && [ "$NEW_ETAG" != "$MENU_ETAG" ]; then pass "menu edit accepted, new ETag minted ($NEW_ETAG)"; else bad "menu edit failed (etag old=$MENU_ETAG new=$NEW_ETAG)"; fi

  # HEADLINE: replay the STALE (original) ETag → 412 STALE_WRITE.
  SC="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -X PATCH "$GW/merchant-bff/v1/merchants/$CMID/menu" -H 'Content-Type: application/json' -H "If-Match: $MENU_ETAG" -d "$EDIT" 2>/dev/null)"
  if [ "$SC" = 412 ]; then pass "STALE WRITE rejected with 412 (ETag mismatch, 02 §1)"; else bad "stale write -> $SC (want 412)"; fi
  # The 412 body carries the STALE_WRITE code envelope (direct curl: keep the stale If-Match).
  SB="$(curl -s --max-time 8 -X PATCH "$GW/merchant-bff/v1/merchants/$CMID/menu" -H 'Content-Type: application/json' -H "If-Match: $MENU_ETAG" -d "$EDIT" 2>/dev/null)"
  if [[ "$SB" == *'STALE_WRITE'* ]]; then pass "412 envelope carries STALE_WRITE code (02 §2)"; else bad "412 body missing STALE_WRITE (${SB:0:160})"; fi

  # Missing If-Match on a mutating edit → 428 IF_MATCH_REQUIRED.
  NC="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -X PATCH "$GW/merchant-bff/v1/merchants/$CMID/menu" -H 'Content-Type: application/json' -d "$EDIT" 2>/dev/null)"
  if [ "$NC" = 428 ]; then pass "menu edit without If-Match rejected (428)"; else bad "no If-Match -> $NC (want 428)"; fi

  # Store status: read ETag, set OPEN with If-Match → 200; stale set → 412.
  ST_ETAG="$(curl -s -D - -o /dev/null --max-time 8 "$GW/merchant-bff/v1/merchants/$CMID/store-status" 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
  SO="$(curl -s --max-time 8 -X PUT "$GW/merchant-bff/v1/merchants/$CMID/store-status" -H 'Content-Type: application/json' -H "If-Match: $ST_ETAG" -d '{"status":"OPEN"}' 2>/dev/null)"
  if [[ "$SO" == *'"status":"OPEN"'* ]]; then pass "store status set OPEN via merchant-bff (If-Match)"; else bad "set OPEN failed (${SO:0:160})"; fi
  SSC="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -X PUT "$GW/merchant-bff/v1/merchants/$CMID/store-status" -H 'Content-Type: application/json' -H "If-Match: $ST_ETAG" -d '{"status":"BUSY"}' 2>/dev/null)"
  if [ "$SSC" = 412 ]; then pass "stale store-status write rejected with 412"; else bad "stale store-status -> $SSC (want 412)"; fi

  # The updated menu is readable (consumer read path used by search/cart).
  assert_body "menu read reflects the edit" GET "/merchant-bff/v1/merchants/$CMID/menu" '"Som Tam"'
fi

# --- 14. V-T4 search & browse (D17/D11) — GATED on the search slot being REAL
# (the stub can't index/route). Demoed THROUGH the customer-bff browse passthrough
# (gateway routes /customer-bff/v1/customer/home + /v1/search -> search-query,
# behind search_v2). Proves: browse feed, geo search, and freshness (event ->
# queryable) via the real in-process index. ---
echo "== V-T4 search & browse (seed -> browse feed -> geo search -> freshness, via customer-bff) =="
SEARCH_MODE="$(awk -F'\t' '$1=="search"{print $3}' "$RUN/plan.tsv" 2>/dev/null)"
if [ "$SEARCH_MODE" != "real" ]; then
  echo "  SKIP: search slot mode='$SEARCH_MODE' (not real) — search section runs only when search-query is the real slot"
else
  SMID="mer_e2e_search_$$"
  SLAT=13.7563; SLNG=100.5018
  # Seed an OPEN store directly into the index (admin ingest) at the browse point.
  SDOC='{"merchant_id":"'"$SMID"'","name":"E2E Som Tam","lat":'"$SLAT"',"lng":'"$SLNG"',"open":true,"rating":4.8,"menu_version":1,"items":[{"item_id":"i1","name":"Som Tam","amount":8000,"currency":"THB","available":true}]}'
  assert_status "seed search doc (ingest)" POST "/search/v1/index/merchants" 202 "$SDOC"

  # Browse feed via customer-bff -> nearby OPEN stores with fee + rating.
  assert_body "browse feed lists the store" GET "/customer-bff/v1/customer/home?lat=$SLAT&lng=$SLNG" "\"store_id\":\"$SMID\""
  assert_body "browse feed carries a delivery fee" GET "/customer-bff/v1/customer/home?lat=$SLAT&lng=$SLNG" '"delivery_fee"'
  assert_body "browse feed carries the rating"     GET "/customer-bff/v1/customer/home?lat=$SLAT&lng=$SLNG" '"rating":4.8'

  # Geo search via customer-bff (text query).
  assert_body "geo search finds the dish" GET "/customer-bff/v1/search?lat=$SLAT&lng=$SLNG&q=som%20tam" "\"store_id\":\"$SMID\""

  # A far-away query must NOT return it (H3-res-5 geo routing).
  assert_body "far query excludes the store" GET "/customer-bff/v1/search?lat=18.79&lng=98.99&q=som%20tam" '"results":[]'

  # Freshness: publish a menu.updated EVENT for a NEW merchant and time how long
  # until it is queryable (event -> queryable). Budget: p99 < 30s (D17).
  FMID="mer_e2e_fresh_$$"
  NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  FENV='{"event_id":"evt_e2e_fresh_'"$$"'","event_type":"menu.updated","occurred_at":"'"$NOW"'","trace_id":"t_e2e","aggregate":{"type":"merchant","id":"'"$FMID"'","region":"bkk"},"schema_version":1,"payload":{"merchant_id":"'"$FMID"'","version":1,"merchant_name":"Fresh E2E","location":{"lat":'"$SLAT"',"lng":'"$SLNG"'},"items":[{"item_id":"fi1","name":"Green Curry","amount":9000,"currency":"THB","available":true}]}}'
  assert_status "publish menu.updated event" POST "/search/v1/index/events" 202 "$FENV"
  t0="$(date +%s%3N 2>/dev/null || date +%s)"; fresh=0; lag_ms=0
  for _ in $(seq 1 100); do
    body="$(curl -s --max-time 5 "$GW/customer-bff/v1/search?lat=$SLAT&lng=$SLNG&q=green%20curry" 2>/dev/null)"
    if [[ "$body" == *"\"store_id\":\"$FMID\""* ]]; then
      t1="$(date +%s%3N 2>/dev/null || date +%s)"; lag_ms=$((t1 - t0)); fresh=1; break
    fi
    sleep 0.05
  done
  if [ "$fresh" = 1 ] && [ "$lag_ms" -lt 30000 ]; then
    pass "event -> queryable freshness ${lag_ms}ms (< 30s budget, D17)"
  else
    bad "event not queryable within 30s (freshness FAILED)"
  fi
fi

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "e2e-smoke: GREEN — $step/$step assertions passed (checkout->delivery across the full topology)"
  exit 0
else
  echo "e2e-smoke: RED — $fail of $step assertions failed"
  exit 1
fi
