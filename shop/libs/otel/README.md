# libs/otel

Tracing bootstrap (04 §2): **real W3C `traceparent` extract/inject** for HTTP,
plus span / `trace_id` accessors that `libs/logging` and `libs/errors` read so
every log line and error envelope carries the live trace.

## Propagation (the load-bearing, fully-tested part)

```go
sc, ok := otel.Extract(r)            // parse incoming traceparent
otel.Inject(outboundHeader, sc)      // continue the trace on an egress hop
ctx, sc := otel.StartSpan(ctx, name) // continue-or-start; fresh child span id
otel.TraceIDFromContext(ctx)         // "4bf92f35…" (04 §3 trace_id)
```

`Middleware` wraps a server: it continues an upstream `traceparent` (or starts a
root), stores the `SpanContext` in the request context, and echoes the active
`traceparent` on the response. Malformed / all-zero / reserved-version headers
are rejected (a fresh root trace is started instead) — see `otel_test.go`.

## Exporter modes

Spans are always created and propagated in-process. Whether they are *shipped*
depends on `OTEL_EXPORTER_OTLP_ENDPOINT`:

- **set** → the service wires the real OTLP exporter at its edge (out of scope
  for this pure-stdlib lib; the `SpanContext` here is what an exporter serialises).
- **empty** → **no-op exporter**: spans created + propagated, not shipped. This
  is the default in tests/CI and any environment without a collector, so nothing
  breaks when Tempo is absent. `otel.ExporterMode()` reports `"noop"`/`"otlp"`.

Zero external dependencies — the W3C format is small and stable; the full OTel
SDK is intentionally not vendored. `StartSpan`/`Middleware` are the seam a
service swaps for the real SDK.
