# Shop platform — S-T1 scaffold Makefile.
# Targets are intentionally thin wrappers over tools/*.sh so the same logic runs
# locally and in CI (ci/run-local.sh).
SHELL := /usr/bin/env bash
GO ?= /usr/local/go/bin/go
KUSTOMIZE ?= $(GO) run sigs.k8s.io/kustomize/kustomize/v5@latest
OVERLAYS := dev preview staging prod

.DEFAULT_GOAL := help

.PHONY: help up down seed smoke test render changed build \
        backdoor-scan security-scan preview preview-isolation strip-test render-preview

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Boot the empty stack (docker-compose, or process fallback) and wait healthy
	@tools/dev-up.sh

down: ## Tear the stack down
	@tools/dev-down.sh

build: ## Compile the gateway + placeholder binaries (prod tags = no testhooks)
	@cd libs/testhooks && $(GO) build ./...
	@cd services/_placeholder && $(GO) build ./...
	@cd gateway && $(GO) build ./...
	@echo "build ok"

seed: ## Seed golden datasets (STUB — real seedctl lands in S-T7)
	@echo "TODO(S-T7): seed SCENARIO=$${SCENARIO:-demo-small} via seedctl + factories."
	@echo "seed: no-op stub (S-T1 ships no business data)."

smoke: ## Run the end-to-end smoke check against the booted stack
	@tools/smoke.sh

test: ## Run scaffold tests (Go vet/build both tags + change-detection fixture)
	@cd libs/testhooks && $(GO) vet ./... && $(GO) vet -tags testhooks ./...
	@cd services/_placeholder && $(GO) vet ./...
	@cd gateway && $(GO) vet ./...
	@tools/changed-paths_test.sh

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
