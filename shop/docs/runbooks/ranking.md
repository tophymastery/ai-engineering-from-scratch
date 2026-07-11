# Runbook — ranking (V-T5: `ranking` service)

Owner: **Discovery** (see `ownership.yaml`). Service: `ranking` (port 8115). Flag:
`ranking_ml` (ships dark; enable per environment). Decision: **D17** (per-cell
OpenSearch retrieval + two-phase ranking; the static fallback doubles as
shed-ladder L1, **D12**).

## What it does

D17 makes discovery **two-phase**: `search` RETRIEVES the top-500 nearby stores
(per-cell OpenSearch), and `ranking` RE-RANKS them to the **top-50** for the
customer browse feed. The gateway routes the browse BFF endpoint
(`GET /v1/customer/home`) to `ranking`, which retrieves candidates from the search
browse contract (`SEARCH_URL`, top-500) and re-orders them. Geo search
(`/v1/search`) stays on `search-query`.

Two ranking modes, selected by `ranking_ml`:

- **ON → ML re-rank.** A feature-weighted model scores each candidate over its
  retrieval-time rating/distance plus **event-fed** popularity + conversion (CTR)
  features. The features come entirely from the `ranking.signal` event stream
  (impression / click / order), consumed exactly-once through the inbox.
- **OFF → static ranking.** The cheap deterministic retrieval order (rating desc,
  distance asc). This is the fallback AND **shed-ladder L1** (D12): under overload
  the controller flips `ranking_ml` off to shed the model cost.

**Auto-fallback.** A health monitor probes the model on a fixed cadence (2 s). On
a model outage it trips a breaker so the feed serves the static order **without a
flag flip** — feed availability stays ≥ 99.9% and the fallback engages < 10 s. A
model error on the hot path also falls back per-request, so a request never fails
because the model is down.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Re-rank latency (top-500 → top-50) | p99 < 50 ms | `RankingRerankLatencyHigh` |
| Feed availability across a model outage | ≥ 99.9% (via auto-fallback) | `RankingFeedAvailabilityLow` |
| Auto-fallback engagement | < 10 s after a model outage | `RankingAutoFallbackEngaged` |
| Feature freshness (signal → feature) | consumer lag p99 < 5 min | `RankingSignalConsumerLag` |

## Key invariants

- **Availability over personalisation.** Every code path (ML, static, breaker
  open, per-request model error) returns a fully-ranked feed. A model outage
  degrades ORDER (ML → static), never availability.
- **Static = shed L1.** `ranking_ml` OFF and the auto-fallback breaker both select
  the exact same static path, so the shed-ladder controller (V-T30) and the
  model-health breaker share one degradation mode.
- **Features are additive aggregates fed exactly-once.** `ranking.signal` events
  are running-increment popularity/CTR signals; the inbox gives exactly-once
  effect, so a redelivered signal never double-counts. Stale features drift the ML
  order toward the static order (safe), never toward wrong money/state.
- **Determinism.** Equal-score ties break by `store_id` asc, so a re-rank is
  reproducible for a fixed candidate set + feature snapshot (01 §6).

## Alert actions

| Alert | First checks |
|---|---|
| `RankingRerankLatencyHigh` | Feature-store read latency; candidate-set size (retrieval returning > 500?); GC. The re-rank is CPU-only over ≤ 500 items — p99 is normally sub-ms. |
| `RankingFeedAvailabilityLow` | Should be impossible while auto-fallback works — check the breaker + the static path; check `SEARCH_URL` retrieval (a retrieval outage surfaces `RANKING_RETRIEVAL_FAILED`, a SEARCH problem, not a ranking one). |
| `RankingAutoFallbackEngaged` | The model-serving path (see model-deploy pipeline below). Expected transiently during a model rollout; sustained ⇒ the model host is down / erroring. Feed is protected (static), personalisation degraded. |
| `RankingSignalConsumerLag` | The `ranking.signal` consumer group / bus lag; ingest-node saturation. Not a correctness risk. |

## Model-deploy pipeline

> **Sandbox disclosure.** This environment has no training/serving infra, so the
> served model is a **deterministic feature-weighted scoring function**
> (`rank/scorer.go`, `DefaultWeights`) standing in for a trained model — clearly
> labelled in code and in `VERIFICATION.md` §V-T5. The pipeline below is the
> DOCUMENTED production process; shipping real weights is a drop-in swap of the
> `ModelWeights` (no code change to the ranker, the feature store, or the fallback).

1. **Offline train + eval.** A training job reads the `ranking.signal` +
   order-outcome tables from the lake (V-T28), trains the ranker, and gates on
   offline metrics (NDCG@50, calibration) vs the current champion. Below-threshold
   ⇒ no candidate.
2. **Package + register.** The candidate model + its feature schema are versioned
   in the model registry with a semantic version and the training-data snapshot id
   (reproducibility, 01 §6). The feature schema is validated against
   `contracts/events/ranking.signal/v1.schema.json` (a feature the model needs but
   the signal does not carry is a contract change → PR the schema, D30).
3. **Shadow.** The candidate serves in **shadow** alongside the champion: it scores
   live traffic, its order is logged but not served, and shadow NDCG/latency are
   compared. Re-rank p99 must stay < 50 ms in shadow.
4. **Canary (flag-gated).** `ranking_ml` is already the on/off gate. A canary
   cohort gets the candidate; auto-rollback (Argo Rollouts) trips on a re-rank-p99
   or feed-availability regression. The model-health breaker protects the feed
   throughout (a bad model host ⇒ static, not an outage).
5. **Promote.** The candidate becomes champion fleet-wide; the previous champion is
   kept one version back for instant rollback.
6. **Rollback.** Flip the model version back (or `ranking_ml` off to force static);
   both are instant and need no redeploy.

## Rollout / rebuild

- **Flag:** `FLAG_RANKING_ML` selects ML vs static (OFF = static fallback = shed
  L1). Ships dark; enable in staging/preview; prod via canary.
- **Feature store is rebuildable.** It is a projection of the `ranking.signal`
  stream — replay the topic to rebuild popularity/CTR from scratch. No
  source-of-truth data lives here.
- **Retrieval dependency.** `SEARCH_URL` must point at the per-cell `search-query`
  Service. A retrieval failure returns `RANKING_RETRIEVAL_FAILED` (502, retryable)
  — that is a search-side incident, handled by `docs/runbooks/search.md`.

## Sandbox note

There is no model-serving/K8s/OpenSearch/Kafka in this environment: the feature
store, scorer, and auto-fallback breaker run in-process (`services/ranking/rank`),
the served model is the deterministic weighted scorer above, retrieval is an HTTP
call to the embedded search slot, the bus is `libs/eventbus`, and the Deployment/
Service topology is render-only (`make render-ranking`). Every V-T5 CORRECTNESS
property (re-rank p99 < 50 ms, auto-fallback < 10 s at ≥ 99.9% availability,
event-fed features, both flag states) is measured for real; only infra scale is
adapted. See `VERIFICATION.md` §V-T5.
