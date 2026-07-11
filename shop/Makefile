# Shop platform — S-T1 scaffold Makefile.
# Targets are intentionally thin wrappers over tools/*.sh so the same logic runs
# locally and in CI (ci/run-local.sh).
SHELL := /usr/bin/env bash
GO ?= /usr/local/go/bin/go
KUSTOMIZE ?= $(GO) run sigs.k8s.io/kustomize/kustomize/v5@latest
OVERLAYS := dev preview staging prod
# Shared libs (S-T3/S-T4/S-T7). Each is its own module; a libs/ change rebuilds
# every importer (change-detection fan-out). testhooks is vetted with both tags
# by the `test` target below.
LIBS := errors otel logging flags idempotency sharding testhooks

.DEFAULT_GOAL := help

.PHONY: help up down seed smoke test test-libs render changed build \
        backdoor-scan security-scan preview preview-isolation strip-test render-preview \
        contract-validate pact-verify

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Boot the empty stack (docker-compose, or process fallback) and wait healthy
	@tools/dev-up.sh

down: ## Tear the stack down
	@tools/dev-down.sh

build: ## Compile the shared libs + gateway + placeholder binaries (prod tags = no testhooks)
	@set -e; for l in $(LIBS); do (cd libs/$$l && $(GO) build ./...); done
	@cd services/_placeholder && $(GO) build ./...
	@cd gateway && $(GO) build ./...
	@cd contracts/registryctl && $(GO) build ./...
	@cd tools/stubgen && $(GO) build ./...
	@echo "build ok"

contract-validate: ## S-T5/D30 merge gate: OpenAPI conventions + event registry + dual-publish example + stubgen (+ asserted red fixture)
	@ci/contract-validate.sh

pact-verify: ## S-T5 merge gate: file-based Pact broker verified against the booted placeholder (+ asserted broken-pact red fixture)
	@ci/pact-verify.sh

seed: ## Seed golden datasets (STUB — real seedctl lands in S-T7)
	@echo "TODO(S-T7): seed SCENARIO=$${SCENARIO:-demo-small} via seedctl + factories."
	@echo "seed: no-op stub (S-T1 ships no business data)."

smoke: ## Run the end-to-end smoke check against the booted stack
	@tools/smoke.sh

test: ## Run scaffold tests (Go vet/build both tags + change-detection fixture) + shared-lib unit tests
	@cd libs/testhooks && $(GO) vet ./... && $(GO) vet -tags testhooks ./...
	@cd services/_placeholder && $(GO) vet ./...
	@cd gateway && $(GO) vet ./...
	@$(MAKE) --no-print-directory test-libs
	@tools/changed-paths_test.sh

test-libs: ## Run the S-T3/S-T4 shared-lib unit tests (errors, otel, logging, flags, idempotency, sharding)
	@set -e; for l in errors otel logging flags idempotency sharding; do \
	  echo "== go test libs/$$l =="; \
	  (cd libs/$$l && $(GO) test ./...); \
	done
	@echo "== flags: per-request override refused in prod / honoured in non-prod =="
	@cd libs/flags && $(GO) test ./... && $(GO) test -tags testhooks ./...
	@echo "== sharding: remap under write load is race-free (D6) =="
	@cd libs/sharding && $(GO) test -race -run TestRemapUnderWriteLoad ./...
	@echo "test-libs: all shared-lib tests passed"

backdoor-scan: ## D29 layer 1: prod build has zero testhook symbols (+ red fixture)
	@ci/backdoor-scan.sh
	@echo "-- red-path fixture (expected FAIL) --"
	@if ci/backdoor-scan.sh --fixture; then echo "ERROR: fixture should have failed" && exit 1; \
	  else echo "fixture correctly failed (red path proven)"; fi

security-scan: ## Dependency/vuln gate (govulncheck, offline fallback)
	@ci/security-scan.sh

strip-test: ## D29 layers 2+3: gateway strips backdoor headers + alerts in prod mode
	@tools/gateway-strip_test.sh

preview-isolation: ## Cross-PR isolation: two tenants, shared baseline, zero bleed
	@tools/preview-isolation_test.sh

preview: ## Simulate a shared preview for a PR (PR=<n>), print URL + cost model
	@tools/preview.sh --pr $${PR:-777}

render-preview: ## Render-validate the shared-preview + GitOps manifests
	@set -e; for f in deploy/preview-shared/baseline.yaml deploy/preview-shared/pr-preview-template.yaml \
	    deploy/gitops/rollout-canary.yaml deploy/gitops/preview-applicationset.yaml; do \
	  ( cd tools/yamlcheck && $(GO) run . $$PWD/../../$$f ); \
	done; echo "render-preview: preview-shared + gitops manifests parsed"

render: ## Render every Kustomize overlay and validate the YAML parses
	@set -e; root=$$PWD; for o in $(OVERLAYS); do \
	  echo "== rendering overlay: $$o =="; \
	  f=$$root/.render-$$o.yaml; \
	  $(KUSTOMIZE) build deploy/overlays/$$o > $$f; \
	  ( cd tools/yamlcheck && $(GO) run . $$f ); \
	  rm -f $$f; \
	done; \
	echo "render: all $(words $(OVERLAYS)) overlays rendered + parsed"

changed: ## Print buildable paths affected since BASE (default: origin/main)
	@tools/changed-paths.sh --base $${BASE:-origin/main}
