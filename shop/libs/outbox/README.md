# libs/outbox

Transactional outbox + log-based CDC relay (S-T6, D8).

- **`SQLStore.WriteInTx(ctx, tx, topic, env)`** — inserts the event into a
  time-partitioned outbox table **in the caller's own transaction**, so the
  business row + idempotency key (D9) + outbox row commit atomically. Engine-
  agnostic over `database/sql`; PG migration with native range partitioning in
  `migrations/0001_outbox.pg.sql`, SQLite for tests.
- **`CDCTailRelay`** — the relay. Tails the outbox by monotonic `id` with a
  durable cursor and publishes to `libs/eventbus`. This is the CDC shape, **not**
  the banned poller:

  | banned (D8) | this relay |
  |---|---|
  | `WHERE published=false` full-table scan | `WHERE id > $cursor ORDER BY id LIMIT n` range scan |
  | `UPDATE published=true` per row → dead tuples → vacuum storms | append-only table + a tiny cursor row |

  Production swaps in Debezium reading the PG WAL
  (`deploy/cdc/debezium-connector.json`); the id-tail is the sqlite/mem stand-in.
  Publish-then-advance-cursor gives at-least-once (crash replays; inbox dedupes).
- **Partition-drop cleanup** — `DropPublishedBefore` drops days fully behind the
  cursor (O(1) `DROP PARTITION` on PG); it **refuses** to drop anything the relay
  has not published (zero event loss).
- **`MemStore`** — in-memory `Source` with identical semantics, used for the
  high-rate soak where a single-writer SQLite outbox would be the bottleneck.

`go test -race ./...`: WriteInTx atomicity, envelope rejection, relay tail +
durable cursor, partition-drop guard + drop.
