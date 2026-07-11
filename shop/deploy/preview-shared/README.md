# Shared multi-tenant preview (D29 / S-T2)

One **baseline** stack (`baseline.yaml`) serves every open PR. A PR does **not**
get its own full stack — it deploys only its **changed services**
(`pr-preview-template.yaml`, rendered per-PR by
`../gitops/preview-applicationset.yaml`) and routes to them with the
`X-Preview-Tenant: pr-<n>` header; anything it did not change falls through to
the baseline.

- **No full-stack-per-PR.** Per-PR marginal cost = changed pods only; the
  baseline is amortized across all PRs. `tools/preview.sh` prints the cost
  ratio (a 1-service PR is 1/30 ≈ 3.3% ≤ the 20% budget).
- **Isolation.** Tenant-scoped state (proven by `tools/preview-isolation_test.sh`)
  gives zero cross-PR data bleed even when two PRs mutate the same entity type.
- **Scale-to-zero + TTL.** Every object carries
  `preview.shop.io/scale-to-zero-idle: 2h` and `preview.shop.io/ttl: 7d`;
  the ApplicationSet prunes overlays on PR close, TTL is the backstop.

Manifests are render-verified here (no live cluster): `make render-preview`.
`../gitops/` holds the Argo Rollouts canary (5→25→50→100 with SLO gate +
auto-rollback) and the per-PR ApplicationSet.
