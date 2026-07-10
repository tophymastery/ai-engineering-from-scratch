# 04 — Operations: CI/CD, Observability, Standardized Logging

The pipeline, telemetry, and log discipline that make the platform safely
deployable many times a day and debuggable across the whole cluster.

## 1. CI/CD

### 1.1 Monorepo layout

```
repo/
├── services/            # Go microservices (one dir each: order/, payment/, …)
├── bffs/                # NestJS BFFs (customer/, merchant/, driver/, admin/)
├── libs/                # shared: errors, idempotency, logging, otel, flags, factories
├── contracts/           # OpenAPI + event schemas + Pact broker config
├── deploy/
│   ├── base/            # Kustomize base manifests per service
│   ├── overlays/        # dev / preview / staging / prod
│   └── applicationsets/ # Argo CD (incl. per-PR preview envs)
├── scenarios/           # seed data (doc 03 §3)
└── tools/               # seedctl, smoke, scripts
```

Path-based change detection builds only affected services (+ dependents via
`libs/` graph); a `libs/` change rebuilds everything that imports it.

### 1.2 Pipeline (every PR)

```
lint + typecheck
  → unit (Go test -race / Jest)
  → contract (Pact verify against broker)
  → build images (multi-arch, SBOM, signed with cosign)
  → integration (Testcontainers: service + real PG/Kafka/Redis)
  → deploy preview env pr-<num> (Argo ApplicationSet)
  → E2E + smoke against preview (seeded scenario, fake providers)
  → security scan (image CVEs, dep audit)
  ⇒ merge allowed only with all gates green
```

### 1.3 Delivery (after merge) — GitOps, progressive

- Merge to `main` ⇒ images pushed, `deploy/overlays/staging` bumped by bot ⇒
  **Argo CD** syncs staging; staging smoke + synthetic orders run continuously.
- Promotion to prod = PR bumping `overlays/prod` (audited, one-click).
- **Argo Rollouts canary** per service: `5% → 25% → 50% → 100%`, each step
  gated on metric analysis (error rate, p99, saga-completion rate). Breach ⇒
  **automatic rollback** to the previous ReplicaSet.
- **DB migrations**: expand/contract only — additive migration ships first
  (gated job before rollout), code that uses it second, destructive cleanup
  ≥1 release later. Rollback is therefore always safe.
- Kafka consumers deploy with paused→resume choreography so a bad version never
  commits offsets it didn't process.
- Full rollout definition of done: any commit on `main` reaches production with
  **no manual steps other than the prod promotion approval**, and any bad
  release self-reverts within one canary window.

## 2. Observability

- **OpenTelemetry SDK in every service and BFF** (shared `libs/otel` bootstrap):
  traces, metrics, logs all tagged with `service`, `version`, `env`, `region`.
- **One trace across the cluster**: W3C `traceparent`/`tracestate` propagated on
  every HTTP/gRPC hop **and injected into Kafka message headers**; consumers
  continue the trace. A customer tap → BFF → order → payment → Kafka →
  dispatch → driver push is a single trace. The edge `request_id` and the
  business keys (`order_id`) are span attributes, so you can pivot either way.
- Collection: otel-collector DaemonSet → Tempo (traces), Prometheus/Mimir
  (metrics), Loki (logs). Tail-based sampling: keep 100% of error traces and
  slow traces, ~1% of healthy ones.
- **Metrics**: RED per endpoint (rate/errors/duration histograms) auto-emitted
  by middleware; USE for infra; business metrics as first-class series:
  `orders_created_total`, `saga_completion_ratio`, `dispatch_time_seconds`,
  `payment_auth_failures_total`.
- **SLOs** (error-budget alerting, multi-window burn rates):
  checkout availability 99.9%, checkout p99 < 800 ms, dispatch assignment p95
  < 5 s, order-tracking freshness < 10 s. Dashboards per service + one
  order-funnel dashboard (created→paid→accepted→delivered conversion, per region).
- **Exemplars** link latency histograms directly to example trace IDs, so a
  dashboard spike is one click from the exact slow trace.
- Every alert links a runbook page; every error response carries its `trace_id`
  (doc 02 §2), so support tickets resolve to traces in one lookup.

## 3. Standardized network logging

One log envelope, emitted by **shared middleware** (`libs/logging`) at the
ingress and egress of every service and BFF — nobody hand-rolls request logs.

```json
{
  "ts": "2026-07-10T02:15:00.123Z",
  "level": "INFO",
  "service": "order",
  "version": "1.42.0",
  "env": "prod",
  "region": "bkk",
  "direction": "ingress",              // ingress | egress | consume | produce
  "protocol": "http",                  // http | grpc | kafka
  "route": "POST /v1/orders",
  "peer": "customer-bff",
  "status": 201,
  "latency_ms": 43,
  "bytes_in": 512, "bytes_out": 1290,
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id": "00f067aa0ba902b7",
  "request_id": "req_01H8…",
  "actor": {"type": "customer", "id": "usr_01H…"},
  "keys": {"order_id": "ord_01H…"},
  "error": null                         // or {"code": "...", "retryable": false}
}
```

Rules:
- **Structured JSON to stdout only**; the cluster pipeline (Loki/Fluent Bit)
  ships them — services never manage log files.
- `request_id` is minted **once at the gateway**, echoed to the client in
  `X-Request-Id`, and propagated on every hop (HTTP header and Kafka envelope) —
  combined with `trace_id`, any request is followable across the entire
  cluster: `{request_id="req_01H8…"}` in Loki returns every hop in order.
- Kafka produce/consume log the same envelope with `protocol: kafka`,
  `route: <topic>`, so async hops are as traceable as sync ones.
- Levels: `DEBUG` (dev only), `INFO` (every request summary), `WARN`
  (degraded/retry), `ERROR` (failed request; always includes `error.code`).
  INFO sampling on ultra-hot paths (location ingest) — errors never sampled.
- **PII discipline**: no names/phones/addresses/tokens in logs; only prefixed
  IDs. Redaction middleware enforces a deny-list; violations fail CI via a
  log-schema test.
- The envelope schema itself is versioned in `contracts/log-schema.json` and
  validated in each service's tests — the standard is enforced, not aspirational.

## 4. Ops runbook seeds

- **Failure drills** (chaos suite, staging, weekly): kill order pods mid-saga
  (expect compensation), partition Kafka (expect lag alert + catch-up), PSP
  timeout storm (expect circuit-open + queued retries).
- **Region evacuation**: geo-partitioned keys (doc 01 §5) mean a region's
  traffic can be re-pointed independently; runbook documents the DNS + config
  flip.
- **On-call golden queries**: order funnel by region, saga stuck > T, consumer
  lag by group, error-budget burn per SLO — all saved dashboards.
