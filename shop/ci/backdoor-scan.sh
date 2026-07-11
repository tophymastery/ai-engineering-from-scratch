#!/usr/bin/env bash
# backdoor-scan.sh — D29 layer-1 enforcement (S-T2).
#
# Builds every Go binary (gateway + services) the way prod ships them — i.e.
# WITHOUT the `testhooks` build tag — and greps the resulting binaries and
# symbol tables for the test-backdoor markers defined in libs/testhooks
# (hooks_enabled.go). A clean prod build contains NONE of them; any hit means a
# backdoor handler leaked into a shippable artifact ⇒ exit 1 (CI red).
#
# Markers (must stay in sync with hooks_enabled.go):
#   - string:  SHOP_TESTHOOK_BACKDOOR_MARKER_v1   (grep -a on the binary)
#   - symbol:  applyBackdoorHooks                 (go tool nm)
# NB: the header NAMES (X-Test-Clock / X-Flag-Override) are intentionally NOT
# scan markers — the gateway legitimately references them to strip them, so
# they appear in a clean prod binary. Only the enabled-build-only markers count.
#
# Usage:
#   ci/backdoor-scan.sh            # scan prod builds  (expect PASS / exit 0)
#   ci/backdoor-scan.sh --fixture  # RED-PATH: build WITH -tags testhooks and
#                                  # prove the scan catches it (expect exit 1)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"

STRING_MARKER="SHOP_TESTHOOK_BACKDOOR_MARKER_v1"
SYMBOL_MARKER="applyBackdoorHooks"

# Buildable Go binary dirs (each is a main package with its own module).
BINARIES=(gateway services/_placeholder)

MODE="prod"
BUILD_TAGS=()
if [[ "${1:-}" == "--fixture" ]]; then
  MODE="fixture"
  BUILD_TAGS=(-tags testhooks)
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "=== backdoor-scan (mode=$MODE) ==="
[[ "$MODE" == "fixture" ]] && echo "  (RED-PATH FIXTURE: building WITH -tags testhooks on purpose)"

found_total=0
for b in "${BINARIES[@]}"; do
  name="$(basename "$b")"
  out="$tmp/$name"
  ( cd "$ROOT/$b" && "$GO" build "${BUILD_TAGS[@]}" -o "$out" . )

  str_hits="$(grep -c -a "$STRING_MARKER" "$out" 2>/dev/null || true)"
  sym_hits="$("$GO" tool nm "$out" 2>/dev/null | grep -c "$SYMBOL_MARKER" || true)"
  hits=$(( str_hits + sym_hits ))
  found_total=$(( found_total + hits ))

  if [[ "$hits" -gt 0 ]]; then
    echo "  [BACKDOOR] $b: string=$str_hits symbol=$sym_hits"
  else
    echo "  [clean]    $b: no testhook markers"
  fi
done

echo "----"
if [[ "$found_total" -gt 0 ]]; then
  echo "backdoor-scan: FAIL — $found_total marker hit(s) in shipped binaries"
  exit 1
fi
echo "backdoor-scan: PASS — prod binaries contain zero testhook markers"
