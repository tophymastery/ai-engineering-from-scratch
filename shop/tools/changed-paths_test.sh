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
# services/merchant-catalog; V-T4 added services/search-indexer + services/search-query;
# V-T6 added services/feed-cache; V-T7 added services/cart; V-T8 added
# services/pricing-promo — a shared-lib change fans out to all of them; V-T9
# added services/order (the saga orchestrator, which imports the same libs);
# V-T10 added services/payment (the money-mutation flagship, same libs);
# V-T11 added services/merchant-queue (CQRS read model, same libs); V-T12 added
# services/dispatch (the zone-owned batch matcher, same libs); V-T13 added
# services/location-gateway (the driver telemetry plane — auth-once + 100ms batch
# ingest, H3 res-7 geo kNN, telemetry tiering — importing the same libs).
got="$(printf '%s\n' 'libs/errors/errors.go' | "$CP" --stdin)"
want="$(printf '%s\n' 'bffs/factories-ts' 'gateway' 'services/_placeholder' 'services/cart' 'services/dispatch' 'services/fakes' 'services/feed-cache' 'services/identity-auth' 'services/identity-profile' 'services/location-gateway' 'services/merchant-catalog' 'services/merchant-queue' 'services/order' 'services/payment' 'services/pricing-promo' 'services/ranking' 'services/search-indexer' 'services/search-query' | sort -u)"
check "libs change = all" "$got" "$want"

# Case 3: docs-only change => nothing.
got="$(printf '%s\n' 'docs/01-architecture.md' | "$CP" --stdin)"
check "docs-only change = nothing" "$got" ""

echo "----"
echo "changed-paths fixture: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
