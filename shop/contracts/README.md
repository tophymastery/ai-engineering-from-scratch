# contracts/

The single integration surface for the monorepo (docs 04 §1.1, 01 §3, and the
TASKS.md parallel-execution rules): OpenAPI specs per service/BFF, event schema
registry, Pact broker config, and `contracts/log-schema.json` (04 §3). Slices
build against *published contracts*, never against another slice's running code,
and contract changes flow through the registry (additive-only per topic; shape
changes become a new `.v2` topic with a dual-publish window — D30). The registry,
broker, and stub generator are built in **S-T5**. S-T1 ships only this
placeholder to reserve the location and document intent.
