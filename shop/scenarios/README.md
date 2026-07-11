# scenarios/

Declarative seed data for the platform (docs 04 §1.1, 03 §3). Each scenario is a
named, deterministic dataset that `seedctl` (arriving in **S-T7**) applies to any
running stack *through public APIs* — e.g. `make seed SCENARIO=lunch-rush`. The
golden datasets `demo-small` and `lunch-rush` land here, with skewed load
datasets (`load-peak-city`, `load-500k-drivers`) added by **V-T31**. Same seed +
scenario must produce a byte-identical dataset on rerun. S-T1 ships only this
placeholder; no scenarios exist yet.
