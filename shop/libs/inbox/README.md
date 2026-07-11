# libs/inbox

Consumer-side exactly-once effect + per-group DLQ (S-T6, D8 + D22).

- **`Processor.Process(ctx, msg, handler)`** — records `event_id` in a
  time-partitioned inbox table **in the same transaction** as the handler's side
  effects. First delivery applies (`applied=true`); any redelivery collides on
  `UNIQUE(consumer_group, event_id)`, rolls back and applies nothing. Delivery is
  at-least-once; the *effect* is exactly-once (01 §3).
- **Skip-inbox rule (D8)** — `ProcessIdempotent` runs a naturally-idempotent
  handler (UPSERT / last-write-wins) **without** an inbox row; the
  `NaturallyIdempotent` marker makes each opt-out auditable in code.
- **`SQLDLQ`** — durable per-consumer-group dead-letter queue implementing
  `eventbus.DLQSink`. The bus parks a message here inline after exhausting
  retries, without blocking the partition. `List` / `Get` / `Depth` / `Replay`;
  `Replay` re-emits via a `Republisher` (the outbox) so reprocessing converges
  exactly-once. Driven by `tools/dlqctl`.
- **Cleanup** — `DropInboxOlderThanRetention` (7-day retention, D8) and
  `DropReplayedDLQBefore`, both partition-drop shaped.
- **`MemProcessor`** — in-memory exactly-once inbox, the throughput stand-in for
  the soak.

Migrations: `migrations/0001_inbox.pg.sql` (native partitioning), SQLite for
tests. `go test -race ./...`: exactly-once (incl. concurrent burst), handler-
failure rollback, skip-inbox rule, retention drop, DLQ park/list/inspect/replay.
