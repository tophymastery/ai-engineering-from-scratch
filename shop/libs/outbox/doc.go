// Package outbox is the transactional outbox + log-based CDC relay for the
// Shop event backbone (S-T6, implements D8).
//
// # Transactional outbox
//
// A service that must both write its DB row and emit an event calls
// WriteInTx(ctx, tx, topic, envelope) INSIDE its own database transaction. The
// business row, the idempotency key (libs/idempotency, D9) and the outbox row
// all commit atomically — no write-then-publish race, no lost events (01 §3).
//
// # Log-based CDC relay (no pollers)
//
// The Relay tails the outbox by a monotonic id with a durable cursor and
// publishes to the eventbus. This is the CDC shape, NOT the banned poller:
//
//	BANNED (D8): SELECT * FROM outbox WHERE published=false  -- full-table scan +
//	             UPDATE published=true per row -> dead tuples -> vacuum storms.
//	THIS RELAY : SELECT ... WHERE id > $cursor ORDER BY id LIMIT n  -- an indexed
//	             range scan on an append-only table, cursor advanced in a tiny
//	             side table. Zero per-row UPDATEs, zero dead tuples.
//
// In production the Relay implementation is Debezium reading the Postgres WAL
// (deploy/cdc/debezium-connector.json); here the CDCTailRelay reads the same
// append-only outbox via the id tail, which is the sqlite/mem equivalent of a
// WAL/rowversion position. Both advance the same durable cursor and both
// publish at-least-once (a crash between publish and cursor-save replays, and
// the consumer inbox dedupes — exactly-once effect).
//
// # Time partitioning + cleanup
//
// The outbox table is RANGE-partitioned by day (native PG partitioning in the
// migration). Cleanup is partition drop of days fully behind the relay cursor
// (DropPublishedBefore) — an O(1) DDL, never a churny DELETE.
package outbox
