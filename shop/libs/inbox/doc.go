// Package inbox is the consumer-side exactly-once effect + per-group DLQ for the
// Shop event backbone (S-T6, implements D8 + D22).
//
// # Exactly-once effect (consumer inbox)
//
// Delivery is at-least-once (the bus may redeliver; the relay may republish
// after a crash). Process records the event_id in a time-partitioned inbox
// table IN THE SAME TRANSACTION as the handler's side effects:
//
//	applied, err := p.Process(ctx, msg, func(ctx, tx) error { ... side effects ... })
//
// The first delivery inserts the id and commits both the row and the effects;
// any redelivery hits UNIQUE(event_id), is recognized as a duplicate, rolls
// back and applies nothing (applied=false). One effect per event (01 §3).
//
// # Skip-inbox rule (D8)
//
// Where a handler is *naturally idempotent* (an UPSERT / last-write-wins
// projection whose re-application is a no-op), the inbox write is pure overhead
// and is skipped via ProcessIdempotent — the documented opt-out. The marker
// lives in code (the ProcessIdempotent call site + the NaturallyIdempotent
// doc-marker) so every opt-out is auditable.
//
// # DLQ (D22)
//
// SQLDLQ implements eventbus.DLQSink: after the bus exhausts a message's
// retries it parks the message here without blocking the partition. tools/dlqctl
// lists/inspects/replays parked events; Replay re-publishes (via the outbox) so
// the reprocess converges exactly-once through Process. Both the inbox and DLQ
// tables are time-partitioned with partition-drop cleanup (7-day inbox
// retention, D8).
package inbox
