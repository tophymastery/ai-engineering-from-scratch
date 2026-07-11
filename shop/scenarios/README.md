# scenarios/

Declarative seed data for the platform (docs 04 §1.1, 03 §3). Each scenario is a
named, deterministic dataset that `seedctl` (arriving in **S-T7**) applies to any
running stack *through public APIs* — e.g. `make seed SCENARIO=lunch-rush`. The
golden datasets `demo-small` and `lunch-rush` land here, with skewed load
datasets (`load-peak-city`, `load-500k-drivers`) added by **V-T31**. Same seed +
scenario must produce a byte-identical dataset on rerun.

**S-T7 shipped** the golden datasets and the seeder:

| Scenario | Shape | Use |
|---|---|---|
| `demo-small.yaml` | 3 merchants ×5 menus, 10 customers, 4 drivers, 8 orders | demos, fast E2E/smoke |
| `lunch-rush.yaml` | 25 merchants ×30 menus, 200 customers, 60 drivers (80% online), 55 orders | standing mid-size peak dataset |

Seed one into any running stack (via public APIs) with
`make seed SCENARIO=lunch-rush`, or build a canonical dump without a target with
`seedctl -scenario scenarios/<name>.yaml -dump-only`. See `tools/seedctl`.
Scenario fields: `seed`, `region`, `merchants{count,menus_each}`,
`customers{count}`, `drivers{count,online_ratio}`, `orders[{count,state}]`.
