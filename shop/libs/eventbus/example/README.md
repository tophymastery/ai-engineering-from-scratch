# libs/eventbus/example — reference sandbox service

The S-T6 reference integration: a tiny service that publishes through the
transactional **outbox → CDC relay → eventbus** and consumes through the
exactly-once **inbox**, end to end — exactly the wiring a real slice (e.g.
order-service building a projection) uses.

```
go run .        # live demo: 200 orders -> published -> consumed -> projection
```

It also hosts the three S-T6 criteria tests (they drive the same wiring):

| test | asserts |
|---|---|
| `TestSoak` | sustains ≥10k events/s through the full path; relay lag p99 < 2s (and max < 2s throughout); **partition drop DURING the soak** with a published==consumed exactly-once audit (zero loss). Duration via `SOAK_SECONDS` (default 8s; the 2h criterion is infeasible here — see VERIFICATION). |
| `TestDuplicateDeliveryBurst` | redelivers every event 10× onto the bus ⇒ zero duplicate side effects (SQL inbox dedupe). |
| `TestPoisonParkAndReplay` | a permanently-failing event parks to the DLQ after 3 retries **without blocking its partition** (following events flow; recovery measured < 60s), then a dlqctl-style replay converges exactly-once. |

```
go test -count=1 ./...              # all three, ~9s
SOAK_SECONDS=60 go test -run TestSoak -count=1 -v ./...   # long soak
```
