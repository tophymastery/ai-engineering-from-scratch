# libs/

Shared libraries imported across services and BFFs (docs 04 §1.1): `errors` (code
registry), `logging` (the 04 §3 log envelope + per-route sampling classes),
`otel` bootstrap, `flags`, `idempotency` (PG-durable, D9), `sharding` (D6), and
`factories` (Go + TS test data). These are built in **S-T3 / S-T4 / S-T7**. A
change under `libs/` triggers a rebuild of **every** buildable module — the
change-detection rule in `tools/changed-paths.sh` encodes exactly this
libs-fan-out. `factories` lands in S-T7; the rest are delivered below.

## Delivered

| Lib | Task | What | Ext deps |
|---|---|---|---|
| [`errors`](errors/) | S-T3 | UPPER_SNAKE code registry + 02 §2 envelope + HTTP mapping | none |
| [`otel`](otel/) | S-T3 | W3C traceparent extract/inject; trace_id accessors; no-op exporter mode | none |
| [`logging`](logging/) | S-T3 | 04 §3 envelope middleware (ingress+egress) + sampling classes; schema-validated | none |
| [`flags`](flags/) | S-T3 | env flags + per-request `X-Flag-Override` (non-prod, testhooks-gated) | testhooks |
| [`idempotency`](idempotency/) | S-T3 | D9 durable dedupe (UNIQUE-in-txn) + cache + migration helper | pg/sqlite **test-only** |
| [`sharding`](sharding/) | S-T4 | D6 256-logical-shard router (hot-reload) + shard-hint ULID codec + online remap tool + sandbox | none |
| [`testhooks`](testhooks/) | S-T2 | D29 backdoor middleware (build-tag guarded) | none |

`errors`/`otel`/`logging`/`flags`/`sharding` are stdlib-only, so services that
ship them add zero external attack surface (`ci/security-scan.sh`).
`idempotency`'s DB drivers
(`lib/pq`, `modernc.org/sqlite`) are imported **only in its tests**; the library
and the reference service compile stdlib-only over `database/sql`. All five are
exercised end-to-end by `services/_placeholder` (`POST /kv`) and unit-tested via
`make test-libs`. See [`../VERIFICATION.md`](../VERIFICATION.md) §S-T3.
