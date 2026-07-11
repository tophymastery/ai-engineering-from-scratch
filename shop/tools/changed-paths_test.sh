#!/usr/bin/env bash
# Fixture test for changed-paths.sh. Three cases pinned to the DoD:
#   1. service-only change  -> just that service
#   2. libs change          -> ALL buildable paths (rebuild-everything rule)
#   3. docs-only change      -> nothing (unaffected paths skipped 100%)
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CP="$DIR/changed-paths.sh"

pass=0
fail=0

check() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    echo "PASS: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name"
    echo "  want: [$want]"
    echo "  got:  [$got]"
    fail=$((fail + 1))
  fi
}

# Case 1: service-only change.
got="$(printf '%s\n' 'services/_placeholder/main.go' | "$CP" --stdin)"
check "service-only change" "$got" "services/_placeholder"

# Case 2: libs change => all buildable paths (sorted). S-T7 added the fake
# providers (services/fakes) and the TS factory mirror (bffs/factories-ts); V-T1
# added services/identity-auth; V-T2 added services/identity-profile; V-T3 added
# services/merchant-catalog — a shared-lib change fans out to all of them.
got="$(printf '%s\n' 'libs/errors/errors.go' | "$CP" --stdin)"
want="$(printf '%s\n' 'bffs/factories-ts' 'gateway' 'services/_placeholder' 'services/fakes' 'services/identity-auth' 'services/identity-profile' 'services/merchant-catalog' | sort -u)"
check "libs change = all" "$got" "$want"

# Case 3: docs-only change => nothing.
got="$(printf '%s\n' 'docs/01-architecture.md' | "$CP" --stdin)"
check "docs-only change = nothing" "$got" ""

echo "----"
echo "changed-paths fixture: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
