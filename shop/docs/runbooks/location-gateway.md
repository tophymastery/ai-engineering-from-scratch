# Runbook — location-gateway (V-T13 Driver telemetry plane; D14/D15)

**Team:** Location · **Slot:** `location-tracking` (port 8109) · **Flag:** `telemetry_v2`

The per-cell gateway drivers stream GPS to. Auth once per connection, 100 ms
batching to the telemetry topic, H3 res-7 geo index (30 s TTL) with a kNN read
dispatch consumes, Flink 1:10 → Iceberg, PG per-trip summaries only.

## SLOs

| SLO | Target | Metric |
|---|---|---|
| Gateway ingest p99 | < 5 ms | `location_gateway_ingest_latency_seconds` |
| Telemetry produce errors | 0 | `location_gateway_produce_errors_total` |
| kNN read p99 | < 10 ms | `location_gateway_knn_latency_seconds` |
| Hottest H3 geo key | < 2% of writes | `location_gateway_hottest_key_fraction` |
| PG location writes | < 500/s per cell | `location_gateway_pg_writes_total` |
| Reconnect-storm recovery | < 60 s | `location_gateway_reconnect_recovery_seconds` |

## Key invariants (why the design holds)

1. **Auth-once per connection.** `Hub.Open` calls `Authenticate` exactly once and
   caches the driver id; `Stream.Push` never authenticates. If ingest p99 climbs,
   the first suspect is auth running per frame — it must not.
2. **100 ms batching.** Frames buffer per stream; a 100 ms flusher coalesces them
   into telemetry-topic batches. Produce errors must be 0 (data-loss risk).
3. **Salted H3 keys.** The physical geo key is `h7_<lat>_<lng>#<0..63>`; a driver's
   salt is a stable hash of its id, so a hot cell's drivers spread across 64
   sub-keys ⇒ hottest key < 2% of writes even if every driver sits in one cell.
4. **Exact kNN.** Ring-expanding search with a geodesic stop bound + a size-k heap:
   returns the true k-nearest (verified vs brute force), O(candidates·log k).
5. **PG carries summaries only.** Raw frames feed the geo index (30 s TTL) and a
   1:10 Iceberg downsample; PG gets ONE row per completed trip. Raw never hits PG.

## Alert responses

- **LocationGatewayIngestLatencyHigh** — check auth-once (must not run per frame),
  buffer lock contention, telemetry produce backpressure. Scale out on connections.
- **LocationGatewayProduceErrors** — telemetry Kafka cluster health/quota (D5),
  broker connectivity, partition availability. Non-zero = potential position loss.
- **LocationGatewayKNNLatencyHigh** — geo-store contention, ring-expansion depth
  (driver density spike), salt fan-out. Dispatch offer latency degrades downstream.
- **LocationGatewayHotKeySkew** — a salted key > 2% of writes: inspect `NumSalts`,
  the salt hash spread, and whether one cell is pathologically dense. Raise salts.
- **LocationGatewayPGWriteRateHigh** — raw positions leaking onto PG (D15 forbids):
  check the 1:10 downsampler and the raw→PG boundary; PG must only get trip closes.
- **LocationGatewayReconnectStormSlow** — a mass reconnect (deploy/blip) is slow:
  auth throughput, stream-registration contention, HPA headroom. Drain gracefully.

## Migration playbook (driver-app protocol migration + kill-switch)

The driver app moves from the legacy per-ping HTTP path to the V-T13 persistent
stream (gRPC bidi, MQTT fallback) behind `telemetry_v2`, with a kill-switch:

1. **Dark deploy.** location-gateway ships with `FLAG_TELEMETRY_V2=false`. Old
   per-ping HTTP path stays primary. New app builds carry both protocols.
2. **Shadow (dual-send).** App sends on BOTH paths; gateway ingests but dispatch
   still reads the old index. Compare kNN parity (new geo index vs old) on a
   sampled cell. No user impact.
3. **Canary flip.** Enable `telemetry_v2` for 1% of drivers in one cell (per-driver
   flag). Dispatch reads the new kNN for those drivers. Watch ingest p99, produce
   errors, kNN p99, hot-key fraction, assignment latency.
4. **Ramp.** 1% → 10% → 50% → 100% per cell, one cell at a time, holding each step
   until the dashboards are green for a full peak window.
5. **KILL-SWITCH.** Set `telemetry_v2=false` (per-cell or global). The gateway
   immediately stops serving the new kNN read; dispatch falls back to the old
   location index (which is kept warm through step 4 via dual-send). Drivers keep
   the legacy HTTP path until the flag returns. No redeploy required — the flag is
   read per-request. Rollback target: < 1 min (flag propagation).
6. **Decommission.** After 100% for two weeks with zero regressions, stop the
   legacy per-ping HTTP path and the old index; drop the dual-send from the app.

Kill-switch drill: flip `telemetry_v2` off under synthetic load, assert dispatch
resumes offering from the fallback index with zero assignment-latency breach, then
flip back on and assert the new kNN re-populates within one 30 s TTL window.
