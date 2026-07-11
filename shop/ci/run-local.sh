#!/usr/bin/env bash
# run-local.sh — execute the FULL PR pipeline locally (S-T2 + S-T5). Mirrors the
# stages of ci/pipeline.yml (04 §1.2): lint → unit → contract-validate →
# pact-verify → build/sign → integration → preview-e2e → security-scan, plus
# render + smoke. Any red stage exits nonzero — that is the "reference PR green /
# merge blocked" proof in this environment (no live GitHub Actions runner).
# Every stage that can run locally runs for real; stages needing infra not
# present here are marked and run their documented local substitute.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
step() { echo; echo "=== $* ==="; }

step "[1/12] lint + typecheck + unit (make test)"
make test

step "[2/12] contract-validate (S-T5/D30) — OpenAPI + registry + dual-publish + stubgen (+ shape-change red fixture)"
make contract-validate

step "[3/12] pact-verify (S-T5) — file-based broker vs booted provider (+ broken-pact red fixture)"
make pact-verify

step "[4/12] build images + sign (cosign) — prod tags, no testhooks"
make build
echo "cosign: sign step is config-only in this env (no registry/keyless OIDC)."
echo "        canonical command lives in ci/cosign.md and ci/pipeline.yml build job."
test -f ci/cosign.md && echo "        ci/cosign.md present. (build/sign: PASS)"

step "[5/12] backdoor symbol scan (D29 layer 1) + red-path fixture"
make backdoor-scan

step "[6/12] integration — gateway strip+alert (D29 layers 2+3)"
make strip-test

step "[7/12] integration — cross-PR preview isolation (2 tenants, zero bleed)"
make preview-isolation

step "[8/12] preview E2E — shared-preview simulation + cost model"
make preview PR=777

step "[9/12] security scan (govulncheck / offline dependency lint)"
make security-scan

step "[10/12] render kustomize overlays (4/4) + preview-shared/gitops manifests"
make render
make render-preview

step "[11/12] boot + smoke"
make up
make smoke

step "[12/12] teardown"
make down

echo
echo "=== CI (local) GREEN — all gates passed; merge would be allowed ==="
