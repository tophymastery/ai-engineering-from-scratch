#!/usr/bin/env bash
# run-local.sh — execute the same stages as ci/pipeline.yml on a dev machine.
# This is the source of truth for "CI is green" until shop/ becomes its own repo
# and pipeline.yml activates as a GitHub Actions workflow.
#
# Stages: lint+build+unit -> change-detection fixture -> render -> smoke.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "=== [1/4] lint + build + unit (make test) ==="
make test

echo "=== [2/4] change-detection self-check (fixture already run in make test) ==="
echo "changed since BASE=${BASE:-origin/main}:"
tools/changed-paths.sh --base "${BASE:-origin/main}" 2>/dev/null || \
  echo "(no git base available in this context — fixture test covers the logic)"

echo "=== [3/4] render kustomize overlays (make render) ==="
make render

echo "=== [4/4] boot + smoke + teardown ==="
make up
make smoke
make down

echo "=== CI (local) green ==="
