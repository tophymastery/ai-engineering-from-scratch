// Package eventbus is the broker abstraction for the Shop event backbone
// (S-T6, implements D8 + the delivery half of D22).
//
// It defines the Publisher/Consumer interfaces the whole platform codes
// against, plus an in-process implementation (MemBroker) that stands in for the
// per-cell Kafka cluster (D5). The interface is deliberately Kafka-shaped —
// topics, partitions keyed by an aggregate key, ordered-within-partition,
// at-least-once delivery with per-consumer-group cursors — so a Kafka-backed
// Broker drops in behind the same interfaces with no caller changes.
//
// Semantics guaranteed by MemBroker (and required of any Broker impl):
//
//   - Ordered per key: messages with the same partition Key land in the same
//     partition and are delivered to a group in publish order.
//   - Partitioned: parallelism == partition count; distinct partitions are
//     consumed concurrently, so one slow/blocked key never stalls another.
//   - At-least-once: a message is redelivered until the handler acks (returns
//     nil) or the message is parked to the DLQ; consumers dedupe the rare
//     double-delivery via libs/inbox (exactly-once *effect*, 01 §3).
//   - DLQ without head-of-line blocking: after MaxAttempts handler failures a
//     message is handed to the DLQSink and the partition cursor advances, so
//     following messages keep flowing (D22).
//
// Every message carries the 02 §4.3 envelope; ValidateEnvelope enforces it
// against the canonical contracts/events/envelope.schema.json (a drift test
// pins the embedded copy to the contract).
package eventbus
