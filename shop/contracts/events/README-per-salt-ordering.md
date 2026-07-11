# Per-salt ordering contract (D11) — merchant fan-out topics

**Owner:** Discovery (V-T4). **Applies to:** the merchant-keyed fan-out topics
`menu.updated`, `store.status_changed`, `rating.updated`. **Decision:** D11
(celebrity-merchant defenses) + D5 (topics keyed by `aggregate_id`).

## The guarantee

At 100M scale a celebrity/chain merchant (a huge menu, a review-bomb, a viral
promo) would pin its entire event stream onto **one** partition — the
`merchant_id` key — and therefore onto one consumer/ingest worker. D11 removes
that hot spot: merchant fan-out topics use **salted keys**

```
merchant_id#<salt>      salt ∈ {0, 1, …, 15}   (16 salts)
```

so a single merchant's events spread across up to 16 partitions.

**Ordering is guaranteed PER SALT, not per merchant.** Two events for the same
merchant that land on **different** salts may be consumed in either relative
order. Two events on the **same** salt keep their published order.

## Why per-salt ordering is sufficient

Every consumer of these topics maintains a **last-write-wins projection** keyed by
a **monotonic `version`** the producer stamps on each event:

- `menu.updated.payload.version` — monotonic menu version.
- `store.status_changed.payload.version` — monotonic status version.
- `rating.updated.payload.version` — monotonic rating-aggregate version.

A consumer applies an event only if its `version` is `>=` the version already
projected for that field; a lower-versioned (out-of-order, cross-salt) event is a
no-op. Convergence therefore does **not** depend on global cross-salt order — only
on each field's monotonic version. The search read model implements exactly this
(`services/search-indexer/index/engine.go`: `applyDoc` LWW by `menuVersion`,
`SetStoreStatus` LWW by `statusVersion`, `ApplyRating`/debouncer LWW by
`ratingVersion`), and it is proven by `TestEngine_StoreStatusLWW`,
`TestEngine_MenuVersionLWW`, and `TestRatingDebounce_LWWCoalesce`.

## Producer rules

1. **Pick a stable salt per document stream.** The search indexer salts by
   *document id* (`SaltForDoc`, a hash of the item/doc id mod 16), so a given
   document always lands on the same salt — its per-document history stays
   ordered. A producer that emits at merchant granularity may salt by
   `hash(merchant_id) % 16` (all that merchant's events on one salt — degenerate
   but valid) or by document id (spread — preferred for chains).
2. **Stamp a monotonic `version`** on every payload. It is the sole ordering
   authority the consumer trusts.
3. **Salt is a routing concern, not a payload field.** It appears only in the
   Kafka partition key (`merchant_id#<salt>`), never inside the event body; the
   `aggregate.id` remains the bare `merchant_id`.

## Consumer rules

1. **Be last-write-wins by `version`.** Never assume cross-salt order.
2. **Be idempotent** (inbox dedupe by `event_id`) — salting does not change the
   at-least-once delivery contract.
3. **Debounce high-frequency aggregates** where the spec requires it: search
   debounces `rating.updated` to ≤1 index write / merchant / 5 min (D17),
   coalescing to the highest-versioned aggregate.

## Balance (measured)

Salting only helps if the 16 salts share load evenly. For a 150k-item chain
merchant the hottest salt partition holds **< 2× the mean** — measured on the real
hash in `services/search-indexer/index/salt_test.go`
(`TestSaltBalance_ChainMerchant`), reported in `VERIFICATION.md` §V-T4.
