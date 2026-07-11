# Image signing (cosign) — build/sign gate config (S-T2, 04 §1.2)

Every image the pipeline builds is multi-arch, ships an SBOM, and is **keyless-
signed with cosign** (Sigstore OIDC) at build time; deploy admission verifies
the signature. In this environment there is no container registry or OIDC
provider, so the signing step is **config-only** (the pipeline renders it; it
does not execute). The canonical steps, wired into `ci/pipeline.yml`
(`build-sign` job) for the extracted repo:

```bash
# 1. build + push multi-arch image (per changed service from changed-paths.sh)
docker buildx build --platform linux/amd64,linux/arm64 \
  -t "$REGISTRY/$SERVICE:$GIT_SHA" --sbom=true --push .

# 2. keyless sign (GitHub OIDC identity, no long-lived keys)
COSIGN_EXPERIMENTAL=1 cosign sign --yes "$REGISTRY/$SERVICE:$GIT_SHA"

# 3. attach the SBOM as an attestation
cosign attest --yes --predicate sbom.spdx.json \
  --type spdxjson "$REGISTRY/$SERVICE:$GIT_SHA"
```

Admission (deploy time) enforces it:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/shop-platform/shop/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$REGISTRY/$SERVICE:$GIT_SHA"
```

Prod images are always built **without** the `testhooks` tag; `ci/backdoor-scan.sh`
runs before signing, so an image carrying a test backdoor never gets signed.
