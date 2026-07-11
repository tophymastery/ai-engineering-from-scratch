# libs/logging

The one **04 §3 network-log envelope**, emitted by shared middleware at the
ingress/egress of every hop — nobody hand-rolls request logs. Structured JSON to
stdout only; the cluster pipeline (Loki/Fluent Bit) ships it.

## Envelope

Every field of 04 §3: `ts, level, service, version, env, region, direction,
protocol, route, peer, status, latency_ms, bytes_in, bytes_out, trace_id,
span_id, request_id, actor, keys, error`. The schema is versioned in
[`contracts/log-schema.json`](../../contracts/log-schema.json) and **validated in
`logging_test.go`** against emitted lines — the standard is enforced, not
aspirational. PII discipline: `actor.id` and `keys` carry prefixed/opaque IDs
only, never names/phones/tokens.

## Usage

```go
lg := logging.New(logging.Config{Service:"order", Version:"1.42.0", Env:"prod",
    Region:"bkk", SampleRate: 0.05}) // 1–5% read-path INFO (D27)

// ingress (server): reads trace_id/span_id from libs/otel; echoes X-Request-Id.
h := otel.Middleware(lg.Middleware(enrich)(mux))

// egress (client): logs outbound calls + injects the trace on every hop.
client := lg.WrapClient(&http.Client{}, "payment")
```

`enrich(r, *Entry)` attaches the `actor` and business `keys` once known.

## Sampling classes (04 §3 / D27)

`DefaultSampler(rate)` keeps **mutations** (POST/PUT/PATCH/DELETE) and **errors**
(status ≥ 400, WARN/ERROR) **always**, and samples **read paths** (healthy
GET/HEAD) at `rate`. Supply your own `Sampler` to keep 100% of a critical read or
drop an ultra-hot one. `Classify(Entry)` exposes the class.

Zero external dependencies — the log-schema test ships a compact stdlib
JSON-Schema (draft-07 subset) validator (`schema_test.go`).
