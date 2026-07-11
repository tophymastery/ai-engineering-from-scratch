#!/usr/bin/env bash
# pii-scan.sh — the V-T2 PII-SCAN merge gate (implements D3).
#
# Two directions, both proven:
#   1. data-inventory + retention registers are well-formed        (GREEN)
#   2. every PII column in the real migrations is registered        (GREEN)
#   3. an UNREGISTERED PII table fixture turns the gate RED         (expected-fail)
#   4. GOLDEN TRAFFIC — the events + logs the REAL service emits    (GREEN)
#      contain zero raw PII (regenerated fresh from the service each run, then
#      scanned against the exact PII strings that were fed in)
#   5. a leaky-traffic fixture turns the gate RED                   (expected-fail)
#   6. crypto-shredding erasure proof: PII unreadable across primary + backup
#      after key destruction while token-only order replay still succeeds
#
# Any real violation, or a red fixture that fails to fail, exits nonzero.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
GO="${GO:-/usr/local/go/bin/go}"
SVC="services/identity-profile"
INV="$ROOT/$SVC/data-inventory.yaml"
RET="$ROOT/$SVC/retention-register.yaml"
MIG="$ROOT/$SVC/migrations/0001_profile.pg.sql"
BIN="$(mktemp -d)"
GOLDEN="$(mktemp -d)"
cleanup() { rm -rf "$BIN" "$GOLDEN"; }
trap cleanup EXIT

sub() { echo; echo "-- $* --"; }

sub "build piiscan + unit-test both directions"
( cd tools/piiscan && "$GO" build -o "$BIN/piiscan" . && "$GO" test -count=1 ./... )
PII="$BIN/piiscan"

sub "validate registers (data-inventory + retention) — expect GREEN"
"$PII" validate "$INV" "$RET"

sub "check-inventory over real migrations — expect GREEN"
"$PII" check-inventory "$INV" "$RET" "$MIG"

sub "UNREGISTERED-table RED fixture (expected-fail — must exit nonzero)"
if "$PII" check-inventory "$INV" "$RET" "$MIG" "$ROOT/tools/piiscan/testdata/unregistered.sql" >/dev/null 2>&1; then
  echo "ERROR: unregistered-table fixture should have failed the inventory gate"; exit 1
else
  echo "inventory RED fixture correctly failed (unregistered PII table => CI red, proven)"
fi

sub "generate GOLDEN TRAFFIC from the REAL identity-profile service"
( cd "$SVC" && "$GO" run . -emit-golden "$GOLDEN" )

sub "scan golden traffic (events + logs) for raw PII — expect GREEN"
"$PII" scan-traffic --known "$GOLDEN/known-pii.txt" "$GOLDEN/events.jsonl" "$GOLDEN/logs.jsonl"

sub "leaky-traffic RED fixture (expected-fail — must exit nonzero)"
if "$PII" scan-traffic "$ROOT/tools/piiscan/testdata/leaky-traffic.jsonl" >/dev/null 2>&1; then
  echo "ERROR: leaky-traffic fixture should have failed the scanner"; exit 1
else
  echo "traffic RED fixture correctly failed (raw PII in an event => CI red, proven)"
fi

sub "crypto-shredding erasure proof (-race): PII unreadable across stores+backups, token replay still works"
( cd "$SVC" && "$GO" test -race -count=1 -run 'TestErasureCryptoShredding|TestEventsAreTokenOnly|TestPIICiphertextAtRest' -v ./... | grep -E 'ERASURE PROOF|PASS|ok|FAIL' )

echo
echo "pii-scan: GREEN — registers valid, golden traffic token-only, erasure crypto-shreds; both RED fixtures correctly reded"
