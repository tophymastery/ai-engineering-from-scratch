# libs/sharding (D6)

Application-level sharding over plain PostgreSQL. A cell has **256 logical
shards** mapped, via a config-driven and **hot-reloadable** table, to **N
physical clusters** (start N=4; split by remapping). An entity key hashes to a
logical shard; the logical shard routes to a physical target. **Prefixed ULIDs
carry a 2-hex-char shard hint** so a point lookup routes from the ID alone — zero
directory reads. Dependency-light: **standard library only** (no external
runtime deps), so any service that adopts it adds zero attack surface
(`ci/security-scan.sh`).

Scope here (S-T4) is the library + remap tool + a runnable in-memory sandbox.
Migrating real service tables onto shards is V-T26 (orders) / V-T27
(payments+ledger); this is the machinery those slices adopt.

## Routing

```go
r, _ := sharding.OpenRouter("routing.json")   // load the map from disk
shard, target := r.RouteKey("cus_01H…")        // hash(key) → shard → physical
shard, target, _ := r.RouteID("ord_a3…")       // point lookup: hint only, no hash
r.Reload()                                     // re-read after an on-disk edit
r.Watch(2*time.Second, onErr)                  // or auto-reload on mtime change
```

`LogicalShard(key) = fmix64(fnv1a64(key)) mod 256`. FNV-1a is fast and
allocation-light; the murmur3 `fmix64` finalizer avalanches the whole 64-bit
digest into the low 8 bits so the power-of-two modulus is uniform (a bare
`% 256` would read only FNV's weakly-mixed low bits). **The hash + finalizer
constants are the cross-language routing contract** — any reimplementation must
reproduce them to stay route-compatible.

## Shard-hint ULID format

```
<prefix>_<HH><BODY>
  prefix : 02 §1 entity prefix (ord, usr, pay, …)
  HH     : 2 LOWERCASE hex chars = the logical shard 00..ff
  BODY   : a standard 26-char Crockford base32 ULID (48-bit ms + 80-bit random,
           monotonic within a millisecond)

example: ord_a301J9Z8P4Q2R7V6X0Y5M3K1BC   →  shard 0xa3 = 163
```

The hint sits **between** the prefix and an otherwise-untouched 26-char ULID, so
the body keeps full ULID randomness, lexicographic time ordering, and
monotonicity — only the routing hint is added.

```go
id := sharding.NewID("ord", customerID)   // hint = LogicalShard(customerID)
shard, prefix, _ := sharding.Decode(id)   // O(1): reads only the 2 hex chars
// Decode(NewID(p, k)) == LogicalShard(k)  for every k  (proven on 1M IDs)
```

## Config (JSON canonical; restricted YAML accepted)

```json
{
  "version": 1,
  "targets": { "pg-0": "host=pg-0 dbname=cell", "pg-1": "…", "pg-2": "…", "pg-3": "…" },
  "assignments": [
    { "target": "pg-0", "shards": "0-63" },
    { "target": "pg-1", "shards": "64-127" },
    { "target": "pg-2", "shards": "128-191" },
    { "target": "pg-3", "shards": "192-255" }
  ]
}
```

`Validate()` enforces that every logical shard `[0,256)` is assigned **exactly
once** and every referenced target exists — a typo fails the load instead of
silently black-holing a shard. A broken on-disk edit is rejected by `Reload()`
and leaves live routing untouched. The YAML dialect is intentionally not a
general parser (dependency-light): it understands exactly
`version` / `targets` / `assignments` (see `testdata/routing.4x256.yaml`).

## Online remap (copy → dual-write → verify → cutover)

`Cluster.Move(shard, to, hooks)` relocates one logical shard online:

1. **copy** — open the dual-write window, then backfill existing rows
   *copy-if-absent* (never clobbers a concurrent dual-write).
2. **dual-write** — every new write to the shard lands on **both** owners,
   paired atomically so they can't diverge.
3. **verify** — briefly freeze the shard and assert `old[shard] == new[shard]`.
4. **cutover** — while frozen, flip the table to the new owner and close the
   window; then drop the shard from the old owner.

Zero misroutes / zero write errors under concurrent load is a property of this
ordering plus the cluster's lock model: every `Put`/`Get` holds a read-lock for
its whole operation, so the two exclusive transitions (enter dual-write,
cutover) can never interleave with a half-completed write. See `cluster.go`.

The **`cmd/remapctl`** tool drives this against the sandbox, optionally under
synthetic write load, and exits non-zero on any error/misroute:

```
go run ./cmd/remapctl -config testdata/routing.4x256.json -shard 100 -to pg-3 \
    -load -writers 8 -duration 2s -seed 2000
```

## Sandbox reference integration

`Cluster` + in-memory `Store`s are the runnable end-to-end sandbox: keys stored
across 4 fake physical targets, routed by the library (`ExampleCluster`,
`TestSandboxRoutesEndToEnd`). In-memory by design so the 1M-scale tests do no
per-key I/O.

## Tested criteria (see `../VERIFICATION.md` §S-T4)

| Test | Criterion | Measured |
|---|---|---|
| `TestDistribution1M` | 1M keys within 1% of uniform (chi-square) | χ²=202.81 < 330.52 (χ²₀.₉₉₉,₂₅₅); ~50 ms |
| `TestShardDeviationUnderOnePercent` | max/min per-shard deviation < 1% | 0.66% at 32M keys; ~1.6 s |
| `TestDecodeAgrees1M` | hint decode == hash routing on 1M IDs | 100.0000%, 256/256 shards; ~0.27 s |
| `TestRemapUnderWriteLoad` | remap under load: zero misroutes/errors | 2819 moves, 0/0; race-clean; ~2 s |

> **Why chi-square, not a <1% per-shard bound, at 1M:** the multinomial standard
> deviation per shard at 1M keys is ≈62 counts ≈1.6% of the 3906 expected, so the
> worst of 256 shards lands ~4% out for **any** uniform hash — a hard statistical
> floor, not a hash defect. The correct 1M uniformity gate is therefore the
> chi-square statistic; the literal <1% per-shard bound is delivered at the N
> (32M) where `1/√N` shrinks it under 1%.
