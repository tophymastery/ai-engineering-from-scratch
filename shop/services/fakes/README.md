# services/fakes — deterministic fake providers (S-T7, 03 §5)

Test doubles that stand in for every external neighbour so no slice ever waits on
a real integration and no test ever hits a paid API or causes an external side
effect. Each is a **std-lib-only Go service, its own module**, boots with zero
config beyond env vars, answers `/healthz`, and runs in `docker-compose.yml` and
the process-mode dev stack alike (`make up`).

| Fake | Port | Contract | What it does |
|---|---|---|---|
| `payment-sim` | 8091 | `contracts/openapi/payment-sim.v1.yaml` | Scriptable PSP: authorize/capture/refund, card `…0002` declines, `…0044` times out, ordered async webhooks, per-day settlement CSV. Seeded RNG ⇒ byte-identical across reruns. |
| `map-sim` | 8092 | `contracts/openapi/map-sim.v1.yaml` | Deterministic routing/ETA: haversine × 1.3, fixed per-mode speed. Zero randomness. |
| `notify-sink` | 8093 | `contracts/openapi/notify-sink.v1.yaml` | Queryable inbox: `/send` captures push/SMS/email, `/inbox` reads back, DELETE clears. |

## Adaptation: `/v1` canonical paths + bare aliases

Platform convention (02 §1) mandates a `/v1` path major-version on every service,
and the `contract-validate` gate enforces it. So each contract documents the
canonical `/v1/...` paths, and each fake serves **both** the `/v1` path and the
bare task-spec alias (`/psp/authorize`, `/route`, `/send`, …) — same handler,
either path. The conformance test verifies the fake against the `/v1` paths the
contract publishes.

## Tests

`make test-fakes` runs all three suites, including the payment-sim **50-rerun
determinism test with `-race`** on the webhook path and the **contract
conformance** test. See each service dir for the specifics.
