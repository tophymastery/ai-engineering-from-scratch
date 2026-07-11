# S-T1 Verification

How each S-T1 Definition-of-Done item and test criterion was verified **in this
environment**, and where the environment forced an adaptation. Legend:
**full** = verified as specified · **adapted** = verified via a documented
substitute · **render-only** = manifests proven correct by rendering, not by a
live deploy.

## Environment realities

- **Go** 1.24.7, **Node** 22, **curl** present. `go run sigs.k8s.io/kustomize/kustomize/v5`
  resolves to Kustomize **v5.8.1**.
- **Docker daemon: NOT available** (`docker info` fails). `docker-compose.yml` is
  shipped as the canonical stack definition, but `make up` detects the missing
  daemon and falls back to a **process-based boot** (`tools/dev-up.sh`): it
  compiles and runs the two std-lib Go binaries directly and health-checks them
  with curl. Observable topology is identical (gateway :8080 proxying
  `/placeholder/*` → placeholder :8081).
- **No Kubernetes cluster.** "Deploys to a cluster" cannot run here; Kustomize
  overlays are instead **verified by render** — all four overlays are built and
  every emitted YAML document is parsed (`tools/yamlcheck`).
- **CI:** no `.github/workflows` created at the repo root (this repo is not the
  shop monorepo). The pipeline lives at `ci/pipeline.yml` (GitHub-Actions-shaped,
  activates when `shop/` is extracted to its own repo) and `ci/run-local.sh` runs
  the identical stages locally.

## DoD / test-criteria matrix

| # | S-T1 requirement | Status | How verified (measured) |
|---|---|---|---|
| DoD-1 | `make up` boots an empty-but-healthy stack (gateway + placeholder) locally | **adapted** | Process-mode boot (Docker daemon absent). `make up` → both `/healthz` return `{"status":"ok",...}`; `make smoke` **3/3 pass** incl. gateway→`/placeholder/*` proxy. |
| DoD-2 | Kustomize base/overlays render for dev/preview/staging/prod; hello-world deploys to a cluster | **render-only** | `make render`: all **4 overlays** build via Kustomize v5.8.1, each emits **4 docs** (2 Deployments + 2 Services), 100% parsed by `tools/yamlcheck`. Live cluster deploy N/A (no cluster). |
| DoD-3 | Change-detection builds only affected paths (verified on a fixture) | **full** | `tools/changed-paths_test.sh` **3/3 pass**: service-only→that service; libs→ALL buildable; docs-only→nothing. |
| Test | Fresh-clone `make up` to healthy in < 10 min | **full (adapted boot)** | Warm boot measured **~0.9 s**; cold-cache build of both binaries **~10.5 s**. Fresh-clone `make up` ≈ tens of seconds ≪ 10 min. |
| Test | CI scaffold green on fixture | **adapted** | `ci/run-local.sh` runs lint+build+unit → change-detection → render → up/smoke/down, all green. `ci/pipeline.yml` mirrors these stages for the extracted repo. |
| Test | Unaffected paths skipped 100% | **full** | docs-only fixture case yields empty output (zero buildable paths); asserted in the fixture test. |

## Commands to reproduce

```
make up      # boot (docker-compose if daemon present, else process fallback)
make smoke   # 3/3 end-to-end checks against the booted stack
make render  # render + YAML-validate all 4 overlays
make test    # go vet + build + change-detection fixture (3/3)
make down    # tear down (verified: both ports closed, processes reaped)
./ci/run-local.sh   # full CI scaffold locally
```

## Deviations summary

1. **Docker unavailable** → `make up` uses a process-based fallback; compose file
   remains the canonical definition. (DoD-1: adapted.)
2. **No K8s cluster** → overlays verified by render + YAML-parse, not live deploy.
   (DoD-2: render-only.)
3. **CI location** → `ci/pipeline.yml` + `ci/run-local.sh` instead of root
   `.github/workflows`, per task instruction.
