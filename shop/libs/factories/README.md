# libs/factories — deterministic typed test-data builders (S-T7, 03 §3)

One factory per core entity, sensible defaults with functional overrides, so
tests and `seedctl` never hand-roll JSON. **Same seed ⇒ byte-identical
entities**, in every process (03 §4 repeatability contract).

```go
f := factories.New(42, factories.Region("bkk"))
m := f.Merchant()
item := f.MenuItem(factories.WithMenuMerchant(m.ID))
o := f.Order(factories.WithStatus("DELIVERED"), factories.WithMerchant(m.ID))
```

Entities: `User` (`usr_`), `Merchant` (`mer_`), `MenuItem` (`itm_`), `Cart`
(`crt_`), `Order` (`ord_`), `Driver` (`drv_`).

## Deterministic IDs

Production `sharding.NewID` uses `crypto/rand` + wall time, so it can't produce
reproducible data. `factories` mints the **same wire format**
(`<prefix>_<HH><26-char Crockford ULID>`) deterministically: the `HH` hint is the
real `sharding.LogicalShard` of a per-entity key, and the body is Crockford-
encoded from a monotonic ms counter + 80 bits from the injected seeded RNG. The
result round-trips through `sharding.Decode` and passes `sharding.ValidateBody`
(asserted in `factories_test.go`), so a factory ID routes identically to a
production ID — yet is identical for a given seed.

A TypeScript mirror with the same shapes/defaults lives at `bffs/factories-ts/`.

`go test ./...` covers: same-seed byte-identity, one-factory-per-entity, valid
shard-hint ULIDs, and override application.
