# libs/idempotency (D9)

Effect-once for mutating endpoints. **D9 corrected round-1**: the source of truth
is a `UNIQUE(idempotency_key)` insert executed **inside the caller's own DB
transaction**, in the same commit as the business write and outbox row. Redis
(here: a pluggable in-memory cache) is a **read-through response cache + IN_FLIGHT
advisory only** — losing it degrades latency, never correctness. The 02 §3 wire
protocol (headers, replay semantics) is unchanged.

> A unique constraint cannot lose an acked write on failover. Async-replicated
> Redis SETNX can — at 580 checkouts/s one failover is thousands of potential
> double charges. So the constraint, not Redis, is authoritative.

## The core: `Manager.Do`

```go
m := idempotency.New(store, cache) // store = SQLStore(db,pg) | MemStore; cache = Redis-like
out, err := m.Do(ctx, key, reqHash, func(ctx, tx idempotency.Execer) (int, []byte, error) {
    // business write on the SAME transaction as the key insert (D9)
    tx.Exec(ctx, "INSERT INTO orders …")
    return 201, body, nil
})
```

`Do` = `BeginTx → INSERT key (UNIQUE) → business(tx) → save response → commit`.
Under N concurrent same-key calls exactly one transaction wins the unique insert
and runs the effect; the losers block on the index, observe the winner's
committed row, and **replay** its stored response. Same key + **different**
`reqHash` ⇒ `409 IDEMPOTENCY_KEY_REUSED`.

## HTTP wire helper (02 §3)

```go
m.HTTP(w, r, logging.TraceIDFromRequest, func(ctx, tx, body []byte) (int, []byte, error) { … })
```

- missing `Idempotency-Key` ⇒ `400 IDEMPOTENCY_KEY_REQUIRED`
- fresh ⇒ run once, persist + return the response
- same key + same body ⇒ replay with `Idempotency-Replayed: true`
- same key + different body ⇒ `409 IDEMPOTENCY_KEY_REUSED`
- concurrent duplicate still settling ⇒ `409 IDEMPOTENCY_IN_PROGRESS` + `Retry-After`

## Stores, cache, engines

- **`SQLStore`** — production path over `database/sql`, engine-agnostic via a
  `Dialect` (`PostgresDialect` for prod; `SQLiteDialect` for pure-Go tests). Same
  UNIQUE-constraint-in-transaction semantics on both.
- **`MemStore`** — transactional in-memory store with a UNIQUE-violation
  *simulation* (per-key gate makes concurrent txns serialise exactly as a DB
  blocks losers on a unique index). Backs the reference service (no DB needed)
  and stands in where no engine is available.
- **`Cache`** — `MemCache` (Redis stand-in) + `SwappableCache` whose `Drop()`
  simulates a Redis failover/FLUSHALL mid-flight for chaos/tests.

## Migration helper (adopting slices)

```go
idempotency.Migrate(ctx, db, idempotency.PostgresDialect{}) // applies the table
sql := idempotency.Schema() // production PG DDL for your own migration runner
```

Production DDL: [`migrations/0001_idempotency.pg.sql`](migrations/0001_idempotency.pg.sql).

## Tested criteria (`concurrency_test.go`, run on Postgres + SQLite + MemStore)

- 100 concurrent same-key ⇒ **exactly 1 effect + 99 replays**
- cache dropped mid-storm ⇒ **still exactly 1 effect**
- same key + different body ⇒ **409 on 100%** of attempts
- cold-cache replay **p99 penalty < +20 ms** (measured, recorded in VERIFICATION.md)
