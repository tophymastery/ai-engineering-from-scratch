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
LIBS := errors otel logging flags idempotency sharding testhooks eventbus outbox inbox factories edgeauth
# S-T7 fake providers (03 §5) — std-lib Go services, own modules.
FAKES := payment-sim map-sim notify-sink

.DEFAULT_GOAL := help

.PHONY: help up down seed smoke test test-libs test-fakes test-seed render changed build \
        backdoor-scan security-scan preview preview-isolation strip-test render-preview \
        contract-validate pact-verify render-events dlqctl-demo \
        e2e-up e2e-down e2e-smoke e2e-swap e2e-sync post-merge-smoke

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Boot the empty stack (docker-compose, or process fallback) and wait healthy
	@tools/dev-up.sh

down: ## Tear the stack down
	@tools/dev-down.sh

build: ## Compile the shared libs + gateway + placeholder + fakes + tools (prod tags = no testhooks)
	@set -e; for l in $(LIBS); do (cd libs/$$l && $(GO) build ./...); done
	@cd services/_placeholder && $(GO) build ./...
	@cd services/identity-auth && $(GO) build ./...
	@cd gateway && $(GO) build ./...
	@set -e; for s in $(FAKES); do (cd services/fakes/$$s && $(GO) build ./...); done
	@cd contracts/registryctl && $(GO) build ./...
	@cd tools/stubgen && $(GO) build ./...
	@cd tools/e2ectl && $(GO) build ./...
	@cd tools/dlqctl && $(GO) build ./...
	@cd tools/seedctl && $(GO) build ./...
	@cd libs/eventbus/example && $(GO) build ./...
	@echo "build ok"

contract-validate: ## S-T5/D30 merge gate: OpenAPI conventions + event registry + dual-publish example + stubgen (+ asserted red fixture)
	@ci/contract-validate.sh

pact-verify: ## S-T5 merge gate: file-based Pact broker verified against the booted placeholder (+ asserted broken-pact red fixture)
	@ci/pact-verify.sh

seed: ## Seed a golden dataset (SCENARIO=demo-small|lunch-rush) into the running stack via PUBLIC APIs
	@set -e; scen="$${SCENARIO:-demo-small}"; url="$${PLACEHOLDER_URL:-http://localhost:8081}"; \
	  test -f scenarios/$$scen.yaml || { echo "seed: no scenarios/$$scen.yaml"; exit 1; }; \
	  if ! curl -fsS --max-time 2 "$$url/healthz" >/dev/null 2>&1; then \
	    echo "seed: stack not up — booting it first"; tools/dev-up.sh; fi; \
	  mkdir -p .run; \
	  echo "seed: applying scenarios/$$scen.yaml to $$url via /kv public API"; \
	  ( cd tools/seedctl && $(GO) run . -scenario ../../scenarios/$$scen.yaml -target "$$url" -out ../../.run/seed-$$scen.json ); \
	  echo "seed: canonical dump -> .run/seed-$$scen.json (pushed to the running stack)"

smoke: ## Run the end-to-end smoke check against the booted stack
	@tools/smoke.sh

e2e-up: ## S-T8: boot the WHOLE shared E2E topology (12 services + 4 BFFs + 3 fakes + gateway) from deploy/e2e/topology.yaml + runtime overlay
	@tools/e2e-up.sh

e2e-down: ## S-T8: tear the shared E2E topology down (add --reset via: tools/e2e-down.sh --reset to clear swaps)
	@tools/e2e-down.sh

e2e-smoke: ## S-T8: checkout->delivery smoke across the full topology (mode-agnostic: stub/fake/real)
	@tools/e2e-smoke.sh

e2e-swap: ## S-T8: swap ONE slot stub->real in the runtime overlay + restart just it, measure wall-time (SVC=<service> [REALCMD="..."])
	@tools/e2e-swap.sh $${SVC:?set SVC=<service>} $${REALCMD:+"$$REALCMD"}

e2e-sync: ## S-T8 post-merge hook: detect slots whose real_cmd binary now EXISTS and swap them into the overlay (GitOps swap-in)
	@set -e; \
	  TOPO=deploy/e2e/topology.yaml; OV=.run/e2e-overlay.yaml; \
	  ( cd tools/e2ectl && $(GO) build -o ../../.run/e2e/bin/e2ectl . ) 2>/dev/null || \
	    ( mkdir -p .run/e2e/bin && cd tools/e2ectl && $(GO) build -o ../../.run/e2e/bin/e2ectl . ); \
	  E=.run/e2e/bin/e2ectl; swapped=0; \
	  while IFS=$$'\t' read -r name port mode contract real_cmd; do \
	    [ -z "$$name" ] && continue; \
	    if [ "$$mode" = real ]; then \
	      "$$E" set-overlay "$$OV" "$$name" real "$$real_cmd"; \
	      echo "e2e-sync: $$name real_cmd exists -> swapped to real ($$real_cmd)"; swapped=$$((swapped+1)); \
	    fi; \
	  done < <("$$E" sync "$$TOPO" "$$OV"); \
	  echo "e2e-sync: $$swapped slot(s) with a merged real_cmd swapped in (overlay=$$OV)"

post-merge-smoke: ## S-T8 merge-webhook target: e2e-sync + up + smoke + down; PAGE the merging team on red (SVC=<merged service>)
	@ci/post-merge-smoke.sh $${SVC:-order}

test: ## Run scaffold tests (Go vet/build both tags + change-detection fixture) + shared-lib + fakes + seedctl tests
	@cd libs/testhooks && $(GO) vet ./... && $(GO) vet -tags testhooks ./...
	@cd services/_placeholder && $(GO) vet ./...
	@cd gateway && $(GO) vet ./...
	@echo "== V-T1 identity & edge auth (D4, -race): edgeauth lib + identity-auth service + gateway auth path =="
	@cd libs/edgeauth && $(GO) test -race -count=1 ./...
	@cd services/identity-auth && $(GO) vet ./... && $(GO) test -race -count=1 ./...
	@cd gateway && $(GO) test -race -count=1 ./...
	@echo "== stubgen + e2ectl (S-T8 topology tooling) vet + test =="
	@cd tools/stubgen && $(GO) vet ./... && $(GO) test -count=1 ./...
	@cd tools/e2ectl && $(GO) vet ./...
	@$(MAKE) --no-print-directory test-libs
	@$(MAKE) --no-print-directory test-fakes
	@$(MAKE) --no-print-directory test-seed
	@tools/changed-paths_test.sh

test-fakes: ## S-T7: fake-provider tests incl. the 50-rerun payment-sim determinism test (-race on the webhook path)
	@echo "== factories: same seed => byte-identical entities; ids are valid shard-hint ULIDs =="
	@cd libs/factories && $(GO) test -count=1 ./...
	@echo "== payment-sim: 50-rerun determinism + ordered webhooks (-race) + contract conformance =="
	@cd services/fakes/payment-sim && $(GO) test -race -count=1 ./...
	@echo "== map-sim: zero-randomness routing/ETA =="
	@cd services/fakes/map-sim && $(GO) test -count=1 ./...
	@echo "== notify-sink: queryable inbox capture/query/clear =="
	@cd services/fakes/notify-sink && $(GO) test -count=1 ./...
	@echo "test-fakes: all fake-provider tests passed"

test-seed: ## S-T7: seedctl determinism — same seed+scenario => byte-identical dataset (in-proc + across two CLI runs)
	@echo "== seedctl: byte-identical dataset on rerun (hash compare) + referential integrity =="
	@cd tools/seedctl && $(GO) test -count=1 ./...
	@echo "== seedctl: byte-identical across two separate CLI process runs (lunch-rush) =="
	@set -e; d=$$(mktemp -d); \
	  ( cd tools/seedctl && $(GO) run . -scenario ../../scenarios/lunch-rush.yaml -dump-only -quiet > $$d/a.json ); \
	  ( cd tools/seedctl && $(GO) run . -scenario ../../scenarios/lunch-rush.yaml -dump-only -quiet > $$d/b.json ); \
	  ha=$$(sha256sum $$d/a.json | cut -d' ' -f1); hb=$$(sha256sum $$d/b.json | cut -d' ' -f1); \
	  echo "  run1=$$ha"; echo "  run2=$$hb"; \
	  [ "$$ha" = "$$hb" ] || { echo "seedctl: dumps differ across runs (non-deterministic)"; exit 1; }; \
	  echo "seedctl: byte-identical across process reruns"; rm -rf $$d

test-libs: ## Run the S-T3/S-T4 shared-lib unit tests (errors, otel, logging, flags, idempotency, sharding)
	@set -e; for l in errors otel logging flags idempotency sharding; do \
	  echo "== go test libs/$$l =="; \
	  (cd libs/$$l && $(GO) test ./...); \
	done
	@echo "== flags: per-request override refused in prod / honoured in non-prod =="
	@cd libs/flags && $(GO) test ./... && $(GO) test -tags testhooks ./...
	@echo "== sharding: remap under write load is race-free (D6) =="
	@cd libs/sharding && $(GO) test -race -run TestRemapUnderWriteLoad ./...
	@echo "== event backbone (S-T6/D8+D22): eventbus + outbox core race-checked =="
	@cd libs/eventbus && $(GO) test -race ./...
	@cd libs/outbox && $(GO) test -race ./...
	@cd libs/inbox && $(GO) test ./...
	@echo "== event backbone criteria: soak(>=10k/s, lag p99<2s, partition-drop) + 10x dedupe + poison->DLQ->replay =="
	@cd libs/eventbus/example && $(GO) test -count=1 ./...
	@echo "== dlqctl park/inspect/replay CLI =="
	@cd tools/dlqctl && $(GO) test -count=1 ./...
	@echo "test-libs: all shared-lib tests passed"

render-events: ## S-T6/V-T1: lint the CDC connector + event-backbone + auth-edge alert templates
	@set -e; for f in deploy/cdc/debezium-connector.json deploy/alerts/event-backbone.yaml deploy/alerts/auth.yaml; do \
	  ( cd tools/yamlcheck && $(GO) run . $$PWD/../../$$f ); \
	done; echo "render-events: Debezium connector + relay-lag/DLQ-depth + auth-edge alerts parsed"

dlqctl-demo: ## S-T6/D22: demo the DLQ park/inspect/replay CLI on a scratch DB
	@set -e; d=$$(mktemp -d)/dlq.db; \
	  ( cd tools/dlqctl && $(GO) run . -db $$d seed \
	    && $(GO) run . -db $$d list -group projection \
	    && $(GO) run . -db $$d inspect 1 \
	    && $(GO) run . -db $$d replay 1 \
	    && echo -n "depth after replay: " && $(GO) run . -db $$d depth -group projection )

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
