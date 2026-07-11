# libs/eventbus

Broker abstraction for the Shop event backbone (S-T6, D8 + delivery half of D22).

`Publisher`/`Consumer`/`Broker` interfaces with **ordered-per-key, partitioned,
at-least-once** semantics, plus `MemBroker` — an in-process implementation that
stands in for the per-cell Kafka cluster (D5). The interface is Kafka-shaped so a
`KafkaBroker` drops in behind it unchanged.

- **Envelope** (`envelope.go`) — the 02 §4.3 event envelope every message
  carries; `ValidateEnvelope` enforces it against the embedded copy of
  `contracts/events/envelope.schema.json` (a drift test pins the copy to the
  contract).
- **DLQ without head-of-line block** — after `MaxAttempts` (default 3) failures a
  message is parked to a `DLQSink` and the partition cursor advances, so later
  messages keep flowing (D22). `MemDLQ` is the in-memory sink for tests;
  `libs/inbox.SQLDLQ` is the durable one.
- **LagRecorder** (`metrics.go`) — quantile recorder behind the "relay lag p99 <
  2s" criterion.

```go
bus := eventbus.NewMemBroker(eventbus.WithPartitions(16))
sub, _ := bus.Subscribe(eventbus.SubscribeConfig{Topic: "order.paid", Group: "projection", DLQ: dlq}, handler)
bus.Publish(ctx, msg)
```

Tests (`go test -race ./...`): ordered-per-key, at-least-once retry, poison
park-without-block, independent consumer groups, envelope validate + schema
drift. The full outbox→relay→bus→inbox soak/dedupe/poison criteria live in
`example/` (the reference sandbox service).
