# contracts/

The single integration surface for the monorepo (docs 04 §1.1, 01 §3, and the
TASKS.md parallel-execution rules). Slices build against *published contracts*,
never against another slice's running code — this directory is what makes the
37 V-slices parallel-safe. Built in **S-T5**; gated in CI by the
`contract-validate` and `pact-verify` stages (ci/run-local.sh stages 2–3).

## Layout

| Path | What |
|---|---|
| `openapi/<name>.v1.yaml` | OpenAPI per service/BFF (seeded: `order.v1`, `customer-bff.v1`). Conventions enforced by `registryctl validate`: `/v1/` paths, snake_case fields, 02 §2 error envelope defined **and** referenced. |
| `events/envelope.schema.json` | The 02 §4.3 Kafka envelope every topic schema embeds. |
| `events/<topic>/<version>.schema.json` | Event schema registry (seeded: `order.created`, `order.paid`, `payment.authorized`, `dispatch.assigned`, `driver.location_updated`). |
| `events/order.paid/deprecation.yaml` + `events/order.paid.v2/` | The worked **D30 dual-publish example**: `total`→`order_total` rename = shape change ⇒ new topic + deprecation date; `order.paid.v2/fixtures/` proves one producer emitting both topics with two consumer generations each green. |
| `pacts/<consumer>__<provider>.json` | **File-based Pact broker** (no pact-broker binary in this env): Pact-v2-shaped interactions, replayed by `registryctl pact-verify` against the *running* provider. Seeded: `customer-bff__placeholder`. |
| `fixtures/registry-red/`, `fixtures/registry-green/`, `fixtures/pact-red/` | CI red/green-path fixtures (expected-fail asserted by the pipeline, like the S-T2 backdoor fixture). |
| `registryctl/` | The Go gate tool — see below. |
| `log-schema.json` | 04 §3 log envelope (S-T3). |

## registryctl (the D30 gate)

```
registryctl validate <contracts-root>       # OpenAPI + registry + dual-publish rules
registryctl diff <old.json> <new.json>      # additive-only check; nonzero on any
                                            # remove/rename/type-change/required-add
registryctl pact-verify <pact.json> <url>   # replay interactions vs live provider
```

**D30 rules enforced:** same-topic changes must be additive-only (new optional
fields). A shape change requires a NEW `<topic>.v2` directory plus a
`deprecation.yaml` (`topic`, `replaced_by`, `deprecation_date`) on the old topic;
`validate` fails if the record is missing, malformed, or the date has passed.

## Stubs for unbuilt neighbours

`tools/stubgen -spec contracts/openapi/order.v1.yaml -port 9090` boots a stub
HTTP server answering every contract path with example/schema-derived responses
(incl. `{param}` templates and `:action` verbs) — a slice develops against it
until the real service lands in the shared E2E env (S-T8).
