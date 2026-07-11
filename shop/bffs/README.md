# bffs/

Backends-for-frontends, one per client (docs 01 §2): `customer/`, `merchant/`,
`driver/`, `admin/` — TypeScript/NestJS, deployed like any other service. A BFF
does aggregation, translation, and client-shaped payloads only: **no business
logic, no data ownership, no direct DB access** — it talks to services via gRPC
with per-call deadlines and circuit breakers, degrading with `warnings[]` when a
non-critical upstream is down. Each BFF is delivered as part of the vertical
slice that needs it (Phase V). S-T1 ships only this placeholder.
