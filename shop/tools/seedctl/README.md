# tools/seedctl — declarative scenario seeder (S-T7, 03 §3)

Reads a `scenarios/*.yaml` scenario and populates a running stack **through
public APIs** (never direct DB inserts — seeds exercise the same code paths as
production). Deterministic: **same seed + scenario ⇒ byte-identical dataset** on
rerun (the canonical JSON dump is the hashable artefact).

```
seedctl -scenario scenarios/lunch-rush.yaml -target http://localhost:8081
seedctl -scenario scenarios/demo-small.yaml -dump-only -out dump.json   # no target
```

Or via Make: `make seed SCENARIO=lunch-rush` (boots the stack if needed, seeds
it, and writes the canonical dump to `.run/seed-<scenario>.json`).

## The sink is pluggable (current adaptation)

`Sink` is an interface. Today the only implementation, `KVSink`, targets the
S-T3 `_placeholder` **`/kv` public API**: each aggregate is `POST`ed as
`{key:"<collection>/<id>", value:<canonical JSON>}` with a deterministic
`Idempotency-Key` (02 §3), so re-seeding converges instead of duplicating. When
the order / catalog / driver slices land, swap in per-entity sinks hitting
`POST /v1/orders`, `POST /v1/merchants`, … — the builder (`build.go`) is
unchanged. `NullSink` is used for `--dump-only` determinism checks.

## Determinism

`Build` constructs entities in a FIXED order (merchants → menus → customers →
drivers → orders+carts) so the seeded factory RNG stream is reproduced exactly.
Referential integrity (orders → users/merchants, dispatched/delivered → drivers)
is deterministic round-robin. `go test ./...` asserts byte-identity on rerun,
seed-sensitivity, and referential integrity.
