# Runbook — feed-cache (V-T6: `feed-cache` service)

Owner: **Discovery** (see `ownership.yaml`). Service: `feed-cache` (port 8116).
Flag: `feed_cache` (ships dark; enable per environment). Decisions: **D11**
(celebrity-merchant defences — two-tier merchant-page cache) and **D17** (geo-tile
feed cache with stampede protection: singleflight + stale-while-revalidate).

## What it does

`feed-cache` fronts the discovery read path with two stampede-safe caches, wired
into the customer-bff browse + merchant endpoints:

- **Geo-tile feed cache (D17).** `GET /v1/customer/home?lat=&lng=` is keyed by a
  **geo tile** (lat/lng quantised to a ~1 km grid) so nearby users share one
  cached feed. Under **stale-while-revalidate**: a fresh tile is served directly;
  a stale tile is served **immediately** while ONE background revalidation runs;
  a cold tile blocks on a single **singleflight**-coalesced origin fetch. The
  origin is the ranking browse feed (D17 two-phase: ranking re-ranks the search
  top-500). The edge CDN fronts this endpoint and honours the SWR directives
  (`deploy/base/feed-cache` annotations).
- **Merchant-page two-tier cache (D11).** `GET /v1/customer/merchants/{id}` is
  served from an **in-process singleflight-coalesced 1 s tier over a Redis 10 s
  tier**. A fresh L1 hit never touches L2; an L1 miss collapses concurrent
  requests via the singleflight and consults L2 before the origin
  (merchant-catalog). Net effect (D11): a **1M-RPS merchant page costs the catalog
  ≤ 1 QPS**, and a cold-key stampede collapses to **exactly one** origin fetch.

The `feed_cache` flag gates behaviour: **ON** = cache; **OFF** = transparent
passthrough to the origin. A request carrying `X-Flag-Override` **bypasses** the
shared cache (deterministic-test requests must neither read nor pollute it) and
the override is forwarded to the origin.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Merchant-page origin (catalog) load | ≤ 1 QPS under any read volume | `FeedCacheMerchantOriginStampede` |
| Geo-tile feed origin per tile | ≤ 1 QPS/tile (singleflight + SWR) | `FeedCacheFeedColdStampede` |
| Geo-tile feed cache hit rate | ≥ 85% at peak | `FeedCacheHitRateLow` |
| Feed background revalidation | error-free (stale-if-error covers gaps) | `FeedCacheRevalidationErrors` |

## Invariants (proven in tests — `services/feed-cache/cache`)

1. **Cold-key stampede ⇒ exactly 1 origin fetch.** 10 000 concurrent requests to
   a cold merchant key ⇒ the origin's atomic counter reads exactly 1 (real
   concurrency, run under `-race`). Same for a cold geo-tile.
2. **Sustained load ⇒ ≤ 1 origin QPS.** Continuous load on one warm key keeps the
   origin at ≤ 1 QPS (L2's 10 s TTL bounds the refresh; L1 misses hit L2, not the
   origin). 1 000 000 requests to one warm merchant page cost the origin 1 fetch.
3. **Feed hit rate ≥ 85%.** Measured over a Zipfian tile-skewed peak profile with
   time-based staleness (fresh + stale-served count as hits under SWR).
4. **Stale-tile stampede ⇒ exactly 1 background revalidation.** A non-blocking
   per-tile guard ensures at most one in-flight revalidation; concurrent stale
   requests serve stale and skip.

## Alert actions

- **`FeedCacheMerchantOriginStampede` (catalog origin > 1 QPS).** The two-tier
  collapse is broken. Check: Redis reachability (L2 down ⇒ every L1 miss reaches
  the origin); L1/L2 TTL config (`MERCHANT_L1_TTL`/`MERCHANT_L2_TTL`); a hot key
  skewing to one node past its singleflight (expected: per-node singleflight still
  bounds origin to ~1/node/L2-TTL). Mitigate: raise L2 TTL, confirm Redis, or
  pin the hot merchant.
- **`FeedCacheFeedColdStampede` (feed origin > 1 QPS/tile).** The feed
  singleflight or SWR is not collapsing. Check the ranking origin latency (a slow
  origin widens the in-flight window) and the revalidation guard.
- **`FeedCacheHitRateLow` (< 85%).** More browse traffic reaches ranking/search
  than budgeted. Check tile skew, the fresh/stale TTL windows, and CDN offload.
- **`FeedCacheRevalidationErrors`.** Stale tiles are served (availability is safe
  via stale-if-error) but revalidation is erroring; check the ranking origin.

## Rollout

`feed_cache` ships **OFF** (transparent passthrough — the feed still serves,
straight from ranking/catalog). Enable per environment: staging/preview flip it
on; prod is a **canary-gated** rollout (watch the hit-rate + origin-QPS panels on
the V-T6 dashboard). Instant rollback: flip `feed_cache` off (passthrough) — no
correctness impact, only origin load rises to the uncached baseline.

## Sandbox adaptations (see `VERIFICATION.md` §V-T6)

- The **Redis 10 s tier** is an in-process TTL store standing in for Redis (no
  daemon in-sandbox) — same TTL contract; the singleflight + two-tier + SWR logic
  is real and fully tested.
- **CDN-fronting** is expressed in `deploy/base/feed-cache` annotations and
  verified **render-only** (`make render-feed-cache`).
- The **1M RPS** scale is adapted to an in-process collapse proof (1M requests ⇒
  1 origin fetch) + a sustained ≤ 1 origin QPS microbench; the exactly-once
  cold-stampede result is **full** (run under `-race`).
