# Runbook — search (V-T4: search-indexer + search-query)

Owner: **Discovery** (see `ownership.yaml`). Services: `search-query` (port 8103,
read path) + `search-indexer` (port 8114, write path). Flag: `search_v2` (ships
dark; enable per environment). Decisions: **D17** (per-cell OpenSearch, H3-res-5
routing, flood control, two-phase ranking) + **D11** (salted merchant keys).

## What it does

The discovery read model (01 §1 `search`): a rebuildable projection (D7/D25 Tier
1) of `menu.updated` + `store.status_changed` + `rating.updated`. `search-indexer`
consumes those merchant events (salted `merchant_id#0..15`, LWW by version) and
maintains a per-cell OpenSearch index (index-per-country, H3-res-5 shard routing).
`search-query` serves geo search (`GET /v1/search`) and the customer browse feed
(`GET /v1/customer/home`, via the customer-bff passthrough), gated by `search_v2`.

## SLOs

| SLO | Target | Alert |
|---|---|---|
| Query latency | p99 < 150 ms | `SearchQueryLatencyHigh` |
| Geo shard fan-out | ≥ 99% of geo queries touch ≤ 2 shards | `SearchShardFanoutHigh` |
| Freshness (event → queryable) | p99 < 30 s | `SearchFreshnessLagHigh` |
| Feed p99 during a 150k reindex | unchanged (±10%) | `SearchIngestBacklogGrowing` |
| Salt balance | hottest salt partition < 2× mean | `SearchSaltPartitionSkew` |

## Key invariants

- **H3-res-5 shard routing (D17).** A geo query routes to the shards covering its
  radius; the shard-tile grouping keeps that to ≤ 2 shards for ≥ 99% of queries.
  If `SearchShardFanoutHigh` fires, a shard tile is mis-sized or a hot cell split
  — check `search_geo_queries_shards_touched_total`.
- **Salted merchant keys (D11).** A chain merchant's documents spread across 16
  salts (`merchant_id#0..15`); ordering is **per-salt**, and every projection is
  **last-write-wins by `version`** (see `contracts/events/README-per-salt-ordering.md`).
  A skew alert means the salt hash is unbalanced.
- **Rating debounce (D17).** `rating.updated` is coalesced to **≤ 1 index write /
  merchant / 5 min**, keeping the latest aggregate. A flood that reaches the index
  (`SearchRatingDebounceIneffective`) is a debounce/clock bug.
- **Bulk-index backpressure (D17).** Reindex writes go to **dedicated ingest
  nodes** with a rate cap; feed reads are **lock-free**, so a 150k-item chain-menu
  reindex never contends with feed reads (feed p99 stays flat). Backlog is
  bounded; sustained backlog delays freshness.

## Alert actions

| Alert | First checks |
|---|---|
| `SearchQueryLatencyHigh` | OpenSearch data-node CPU/heap; shard fan-out (a routing regression multiplies scanned shards); ranking timeouts. |
| `SearchFreshnessLagHigh` | Indexer consumer lag by group; ingest-node saturation; a stuck salt partition. Rebuild is safe (projection is replayable from events). |
| `SearchShardFanoutHigh` | `search_geo_queries_shards_touched_total{buckets=">2"}`; recently changed shard-tile size or cell density. |
| `SearchIngestBacklogGrowing` | A large chain reindex in flight (expected, transient); ingest-node capacity; raise the rate cap or add ingest nodes. |
| `SearchSaltPartitionSkew` | The salt distribution (`search_salt_partition_docs`); a pathological merchant/doc-id pattern. |

## Rollout / rebuild

- **Flag:** `FLAG_SEARCH_V2` gates the public surface (reads return 404
  `SEARCH_DISABLED` when dark). Enable in staging/preview; prod via canary.
- **Rebuild:** the index is a rebuildable projection — replay `menu.updated` /
  `store.status_changed` / `rating.updated` from the event store into a fresh
  index (tested end-to-end in V-T34). No source-of-truth data lives here.

## Sandbox note

There is no OpenSearch/K8s in this environment: the inverted index + shard router
run in-process (`services/search-indexer/index`), and the OpenSearch data/ingest
node topology is render-only (`make render-search`). The routing, salting,
debounce and backpressure LOGIC is the same in-process code, tested for real. See
`VERIFICATION.md` §V-T4.
