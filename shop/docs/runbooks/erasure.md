# Runbook — Right-to-erasure via crypto-shredding (identity-profile, V-T2 / D3)

**Owner:** Identity & Trust · **Service:** `identity-profile` · **Decision:** D3
**Rehearsed by:** `services/identity-profile` `TestErasureCryptoShredding` (unit,
`-race`) and `ci/pii-scan.sh` (live emit + scan + erasure proof) — both run in
CI on every PR.

## What & why

Customer PII (name, phone, email, address) lives **only** in the owning
jurisdiction's per-cell store (in-country for ID/VN), always **encrypted at rest**
with AES-256-GCM under a **per-user data-encryption key (DEK)**. The DEK is stored
once, **wrapped by the master KEK**, in the cell keystore (`data_keys`).

Right-to-erasure against an append-only / backed-up substrate is impossible by
"deleting rows" — the data survives in WAL, snapshots and immutable backups. So
erasure is **crypto-shredding**:

> **Destroy the per-user DEK. Every PII ciphertext — primary, replica, immutable
> backup, any pre-token event copy — is then permanently unreadable, without
> touching those immutable stores.** The `usr_`/`adr_` tokens remain valid, so
> token-keyed order history still replays.

**SLA: PII unreadable across stores + backups within 72 h.** (Enforced by
`retention-register.yaml` `erasure.sla_hours: 72` and the
`ProfileErasureSLABreached` alert.)

## SLOs this protects

| SLO | Target | Dashboard / alert |
|---|---|---|
| Erasure completion | ≤ 72 h | `deploy/dashboards/profile.json` p1 · `ProfileErasureSLA{Approaching,Breached}` |
| Erasure success | no failures | p2 · `ProfileErasureFailures` |
| Residency | PII only from owning cell | p3 · `ProfileResidencyViolations` + NetworkPolicy |
| Crypto availability | KEK up, live PII decryptable | p4 · `ProfileKEKUnavailable`, `ProfilePIIDecryptErrors` |

## Erasure procedure

1. **Intake.** An erasure request (account deletion / DSAR) arrives with the
   `usr_` token and the owning cell (jurisdiction). Confirm the request is
   authorised (DPO / support tooling).

2. **Execute in the owning cell.** `POST /v1/profiles/{usr}:erase` with
   `X-Cell: <jurisdiction>` (via the customer-bff or admin plane). This runs, in
   one transaction against the owning cell:
   - `UPDATE data_keys SET wrapped_dek = NULL, destroyed_at = now()` — **the
     shred**. The only decryptable copy of the DEK is gone.
   - stamp `erased_at` + null the primary ciphertext columns,
   - emit a **token-only** `profile.erased` event (downstreams drop cached PII).
   Then the backup tombstone (`erased_at`) is stamped. The immutable backup
   ciphertext is **left in place** — it is already unreadable.

3. **Verify unreadable.** `GET /v1/profiles/{usr}` → **410 PROFILE_ERASED**. A
   direct decrypt of the backup ciphertext now fails with `errKeyDestroyed`
   (asserted by `TestErasureCryptoShredding`).

4. **Verify tokens survive.** `GET /v1/tokens/{usr}` → `exists:true, erased:true`;
   an order-replay from a token-only snapshot still succeeds (`/v1/orders:replay`).
   Order history is **not** destroyed — only the PII is.

5. **Record.** Log the erasure receipt (`user_token`, `erased_at`,
   `stores:[primary,backup]`, `key_destroyed:true`) to the audit trail and close
   the request. Keystore backups are rotated within their own ≤ 72 h window so no
   older wrapped-DEK copy survives (they are the crypto-shred target, not the PII).

### Rollback

There is **no rollback** — crypto-shredding is intentionally irreversible (that
is the erasure guarantee). If an erasure was executed in error, the user must
re-onboard and re-enter their PII; the prior ciphertext is unrecoverable.

## Residency note (non-owning-cell access)

Erasure and all PII reads happen **only in the owning cell**. The
`deploy/base/identity-profile/networkpolicy.yaml` denies non-owning-cell access at
L3; the service also returns `403 PROFILE_RESIDENCY_VIOLATION` for a request
tagged with a cell it is not homed for. Never fetch PII cross-cell — route the
request to the owning cell's identity-profile.

---

## DPO sign-off record

| Field | Value |
|---|---|
| Control | Right-to-erasure via crypto-shredding (D3) |
| Service | `identity-profile` (V-T2) |
| Mechanism reviewed | Per-user AES-256 DEK, KEK-wrapped keystore; erasure destroys the DEK across primary + backups; PII ciphertext rendered unreadable; `usr_`/`adr_` tokens retained for order replay |
| Registers reviewed | `services/identity-profile/data-inventory.yaml`, `services/identity-profile/retention-register.yaml` (CI-validated by `tools/piiscan`) |
| SLA | PII unreadable across stores + backups ≤ 72 h |
| Residency | Per-jurisdiction stores (in-country ID/VN); NetworkPolicy + app guard deny non-owning-cell access |
| Evidence | `TestErasureCryptoShredding` (unit, -race) + `ci/pii-scan.sh` golden-traffic scan (zero raw PII in events/logs) |
| **DPO sign-off** | **Approved — R. Meyer, Data Protection Officer, 2026-07-11** |
| Review cadence | Re-review on any change to the erasure path, keystore, or registers |
