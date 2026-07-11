#!/usr/bin/env bash
# post-merge-smoke.sh — the S-T8 POST-MERGE automation hook (the merge-webhook
# target). On every merge to the shared E2E env it:
#   1. e2e-sync   — swap in any slice whose real binary now exists (merged impl)
#   2. e2e-up     — boot the whole topology (manifest + overlay)
#   3. e2e-smoke  — run the checkout->delivery smoke
#   4. e2e-down   — tear down
# On RED it emits a single PAGE line naming the MERGING team (looked up from
# ownership.yaml by the merging service). In production this script is the
# GitHub merge-webhook / GitOps post-sync hook; here it runs the identical steps
# locally. The merging service is passed as $1 (the webhook payload's changed
# service) and defaults to `order` for a manual run.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
MERGED_SVC="${1:-order}"
OWN="$ROOT/ownership.yaml"

# team_for <service> — look up the owning team from ownership.yaml.
team_for() {
  local svc="$1"
  awk -v s="$svc" '
    $0 ~ "^[[:space:]]*"s":" {
      if (match($0, /team:[[:space:]]*"[^"]*"/)) {
        t = substr($0, RSTART, RLENGTH); sub(/team:[[:space:]]*"/, "", t); sub(/"$/, "", t); print t; exit
      }
    }' "$OWN"
}

echo "== post-merge smoke for merged service: $MERGED_SVC =="

# 1. detect merged implementations and swap them in (writes the overlay).
make --no-print-directory e2e-sync || true

# 2+3. boot + smoke.
smoke_rc=0
if make --no-print-directory e2e-up; then
  make --no-print-directory e2e-smoke || smoke_rc=$?
else
  smoke_rc=1
fi

# 4. always tear down.
make --no-print-directory e2e-down >/dev/null 2>&1 || true

team="$(team_for "$MERGED_SVC")"
[ -n "$team" ] || team="UNKNOWN (add $MERGED_SVC to ownership.yaml)"

if [ "$smoke_rc" -eq 0 ]; then
  echo "post-merge-smoke: GREEN — $MERGED_SVC merge is healthy in the shared E2E env (team: $team)"
  exit 0
else
  # The paging line an alerting rule keys on (routes to the merging team).
  echo "PAGE team=\"$team\" service=\"$MERGED_SVC\" reason=\"post-merge E2E smoke RED\" runbook=\"make e2e-up && make e2e-smoke\""
  echo "post-merge-smoke: RED — paged $team for $MERGED_SVC"
  exit 1
fi
