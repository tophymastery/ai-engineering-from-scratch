# @shop/factories (TypeScript mirror)

TypeScript mirror of `libs/factories` (S-T7, 03 §3): typed builders with seeded
defaults + overrides for the core entities — `User`, `Merchant`, `MenuItem`,
`Cart`, `Order`, `Driver` — using the platform `usr_/mer_/itm_/crt_/ord_/drv_`
shard-hint ULID prefixes.

```ts
import { New } from "@shop/factories";

const f = New(42, { region: "bkk" });
const m = f.merchant();
const item = f.menuItem({ merchant_id: m.id });
const o = f.order({ status: "DELIVERED", merchant_id: m.id });
```

## Status: source-checked-in, compiles when BFF tooling arrives

This package is **plain TypeScript with zero runtime dependencies**. You do not
need to `npm install` anything to read or review it. It compiles the moment the
first BFF slice (Phase V) brings `tsc`/NestJS into the toolchain:

```
npx tsc -p bffs/factories-ts/tsconfig.json    # emits dist/ (once tsc is available)
```

Until then it is checked in as source and kept in lockstep with the Go
`libs/factories` (same entity shapes, same default values, same override
ergonomics).

## Determinism scope

`New(seed)` is byte-reproducible **within TypeScript** (mulberry32 RNG). It is
deliberately **not** required to be byte-identical to the Go output: the Go
`tools/seedctl` is the single canonical dataset generator (03 §3), and the
cross-language contract here is the **entity shape + defaults**, so a BFF written
against these types lines up with services seeded by the Go factories. The
shard-hint prefix + `logicalShard` hash mirror `libs/sharding` exactly (FNV-1a +
murmur3 fmix64), so a TS-minted id routes to the same logical shard as the Go one.
