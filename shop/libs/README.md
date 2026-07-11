# libs/

Shared libraries imported across services and BFFs (docs 04 §1.1): `errors` (code
registry), `logging` (the 04 §3 log envelope + per-route sampling classes),
`otel` bootstrap, `flags`, `idempotency` (PG-durable, D9), `sharding` (D6), and
`factories` (Go + TS test data). These are built in **S-T3 / S-T4 / S-T7**. A
change under `libs/` triggers a rebuild of **every** buildable module — the
change-detection rule in `tools/changed-paths.sh` encodes exactly this
libs-fan-out. S-T1 ships only this placeholder.
