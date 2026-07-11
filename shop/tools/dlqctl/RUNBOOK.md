# Runbook — DLQ park / inspect / replay (`dlqctl`)

Implements the D22 operator tooling: every consumer group has a dead-letter
queue; `dlqctl` lists, inspects, and replays parked events. This is the
companion to the **relay-lag** and **DLQ-depth** alerts in
`deploy/alerts/event-backbone.yaml`.

## When you are paged

- **`EventBackboneDLQDepthHigh`** — a consumer group's DLQ has parked events
  (poison messages, a handler bug, a downstream outage). The partition is NOT
  blocked (parked events don't stall the stream, S-T6 / D22), so this is not a
  latency page — it is a correctness backlog to drain.
- **`EventBackboneRelayLagHigh`** — the CDC relay is behind. Check Debezium /
  the relay pod, Kafka health, and consumer throughput. DLQ replay is unrelated
  to this alert.

## Triage flow

```
# What is parked, and for which group?
dlqctl -db $CONSUMER_DB depth  -group <group>
dlqctl -db $CONSUMER_DB list   -group <group> -status parked

# Look at one event: envelope + failure cause + attempt count.
dlqctl -db $CONSUMER_DB inspect <id>
```

Read the `cause`. Decide:

1. **Transient/downstream** (the dependency is healthy again) → replay as-is.
2. **Handler bug** → ship the fix FIRST (the handler must now succeed), then
   replay. Replaying into a still-broken handler just re-parks after 3 retries.
3. **Genuinely bad event** (schema violation, un-processable) → leave parked and
   escalate; do not replay. Retain for audit; it is dropped only after the DLQ
   retention window once marked replayed.

## Replay

```
dlqctl -db $CONSUMER_DB replay <id>                 # one event
dlqctl -db $CONSUMER_DB replay -group <group> -all  # drain the whole group
```

Replay **re-emits the event through the outbox**, so it travels the normal
`outbox → relay → bus → inbox` path. Because the consumer inbox dedupes on
`event_id`, replay **converges exactly-once**: if the original effect never
landed (the usual case — the failing handler rolled back its inbox row), it is
applied once now; if somehow it had landed, the inbox drops the replay. Replay
is itself idempotent — an already-`replayed` row is a no-op, and replaying the
same event twice cannot double-apply.

## Verifying recovery

```
dlqctl -db $CONSUMER_DB depth -group <group>   # -> 0 when drained
```

Confirm the consumer's projection/side-effect reflects the replayed events, and
that `EventBackboneDLQDepthHigh` clears.

## Notes

- `-db` is the consumer group's database (PostgreSQL in a cell). In the sandbox
  it is a SQLite file; `dlqctl -db /path seed` parks two sample events so the
  flow is demoable without infrastructure.
- **Durable timers (D22 context):** saga timeouts (`T_accept`, `T_dispatch`,
  capture-by) live in the order DB's durable timer table fired by a leased
  sweeper. They share this backbone's replay-safety property but are owned by
  the order/saga slice (V-T9), not by `dlqctl`.
