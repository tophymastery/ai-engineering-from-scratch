#!/usr/bin/env bash
# contract-validate.sh — the S-T5 CONTRACT-VALIDATE merge gate (implements D30).
#
# Static contract gates over contracts/ (the single integration surface):
#   1. compile + unit-test the platform tools (registryctl, stubgen)
#   2. registryctl validate — OpenAPI conventions (02 §1) + event envelope
#      (02 §4.3) + D30 dual-publish/deprecation rules  (GREEN on real contracts)
#   3. additive-only diff GREEN fixture               (exit 0 asserted)
#   4. in-place topic shape-change RED fixture         (exit != 0 asserted — the
#      "in-place shape change => CI red" test criterion, like the S-T2 backdoor fixture)
#   5. .v2 dual-publish worked example                 (both consumer gens green)
#   6. stubgen boots order.v1 + 2 curls                (runnable stub from a contract)
#
# Any real violation, or a red fixture that fails to fail, exits nonzero.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
GO="${GO:-/usr/local/go/bin/go}"
BIN="$(mktemp -d)"
STUB_PID=""
cleanup() { [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true; rm -rf "$BIN"; }
trap cleanup EXIT

sub() { echo; echo "-- $* --"; }

sub "build + unit-test tools (registryctl, stubgen)"
( cd contracts/registryctl && "$GO" build -o "$BIN/registryctl" . && "$GO" test ./... )
( cd tools/stubgen        && "$GO" build -o "$BIN/stubgen" .     && "$GO" test ./... )
REG="$BIN/registryctl"

sub "registryctl validate (real contracts — expect GREEN)"
"$REG" validate "$ROOT/contracts"

sub "additive-only diff GREEN fixture (expect exit 0)"
"$REG" diff \
  "$ROOT/contracts/events/order.created/v1.schema.json" \
  "$ROOT/contracts/fixtures/registry-green/order.created.additive.schema.json"

sub "in-place shape-change RED fixture (expected-fail — must exit nonzero)"
if "$REG" diff \
     "$ROOT/contracts/events/order.created/v1.schema.json" \
     "$ROOT/contracts/fixtures/registry-red/order.created.inplace-shape-change.schema.json" \
     >/dev/null 2>&1; then
  echo "ERROR: in-place shape-change fixture should have failed the registry gate"; exit 1
else
  echo "registry RED fixture correctly failed (D30 shape-change gate proven)"
fi

sub ".v2 dual-publish worked example (both consumer generations green)"
( cd contracts/events/order.paid.v2/fixtures && "$GO" test ./... )

sub "stubgen boots order.v1 + 2 curls"
"$BIN/stubgen" -spec "$ROOT/contracts/openapi/order.v1.yaml" -port 19191 >"$BIN/stub.log" 2>&1 &
STUB_PID=$!
for _ in $(seq 1 40); do curl -fsS --max-time 1 http://localhost:19191/v1/orders/ord_probe >/dev/null 2>&1 && break; sleep 0.25; done
body1="$(curl -fsS -X POST http://localhost:19191/v1/orders -H 'Idempotency-Key: idem_x' -d '{"quote_id":"q","payment_method_id":"pm"}')"
body2="$(curl -fsS http://localhost:19191/v1/orders/ord_01H8XG)"
[[ "$body1" == *'"status":"PAYMENT_PENDING"'* ]] || { echo "stub POST /v1/orders wrong: $body1"; exit 1; }
[[ "$body2" == *'"order_id"'* ]]                || { echo "stub GET /v1/orders/{id} wrong: $body2"; exit 1; }
echo "stub curl 1 (POST /v1/orders): $body1"
echo "stub curl 2 (GET  /v1/orders/{id}): $body2"

echo
echo "contract-validate: GREEN — OpenAPI + registry + D30 + dual-publish + stubgen all pass"
