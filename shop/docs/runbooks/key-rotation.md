# Runbook — ES256 signing-key rotation (identity-auth, V-T1 / D4)

**Owner:** Identity & Trust · **Service:** `identity-auth` · **Decision:** D4
**Rehearsed by:** `tools/rotate-keys-demo.sh` (live) and
`services/identity-auth` `TestKeyRotationRunbook` (unit) — both run in CI.

## What & why

Access tokens are 15-minute **ES256** JWTs, verified at the gateway from a
**cached JWKS** (`GET /.well-known/jwks.json`) — there is **no call to
identity-auth on the request hot path** (D4). Rotation therefore has one hard
rule:

> **Publish the new key in JWKS *before* signing with it, and retire the old
> key only *after* every token it signed has expired (≥ 15 min).**

JWKS carries **at most 2 keys** so there is always exactly one overlap window.

## SLOs this protects

| SLO | Target | Dashboard / alert |
|---|---|---|
| Edge verify latency | p99 < 1 ms | `deploy/dashboards/auth-edge.json` p1 · `AuthEdgeVerifyLatencyHigh` |
| Revocation lag | ≤ 30 s | p2 · `AuthRevocationLag{High,Critical}` |
| Authed-traffic availability | decoupled from identity-auth uptime | p5 · (invariant) |
| JWKS availability | keys always cached at the edge | p4 · `AuthJWKSFetchFailing`, `AuthJWKSEmpty` |

## Rotation procedure

Endpoints are ops-only (non-prod / admin plane; disabled when `ENV=prod` in the
public build). In production drive them from the identity control plane.

1. **Pre-flight.** Confirm the edge is healthy: `AuthJWKSFetchFailing` is quiet
   and `gateway_jwks_keys >= 1` on every cell. Note the current `primary_kid`
   from `GET /healthz`.

2. **Add key B (publish before signing).**
   `POST /v1/auth/keys:rotate` → generates key B, adds it to JWKS, makes it the
   **primary signer**. JWKS now advertises **A + B**.
   - Verify: `GET /.well-known/jwks.json` lists 2 kids.
   - Verify at the edge: a gateway picks up B within ≤ 1 s of the first
     B-signed token (throttled unknown-kid JWKS refresh). Watch p4.

3. **Overlap window (do nothing for ≥ 15 min).** Tokens minted before step 2
   were signed by **A** and MUST keep verifying — A is still in JWKS. New tokens
   are signed by **B**. This is the no-forced-logout guarantee. Wait until the
   longest-lived A token has expired (access TTL = 15 min; wait 20 min for
   safety).

4. **Retire key A.** `POST /v1/auth/keys:retire` → drops the oldest key (A).
   JWKS now advertises **B only**. Refuses to retire the primary or the last
   key. A **freshly-rolled edge** no longer honours A-signed tokens (all long
   since expired); edges that cached A will drop it on their next JWKS refresh /
   restart.
   - Verify: JWKS lists 1 kid (B); `AuthEdgeRejectRateHigh` did **not** spike
     (no valid tokens were still relying on A).

5. **Post-checks.** `primary_kid` == B on `/healthz`; reject-rate flat; verify
   latency still < 1 ms p99.

### Rollback

If step 2 or 3 shows a spike in `AuthEdgeRejectRateHigh` (a bad key or an edge
that can't fetch JWKS): **do not retire anything.** Investigate JWKS
reachability first. Because A is still primary-eligible until step 4, simply
re-issuing via A (or re-rotating to a fresh B) restores service; retiring is the
only irreversible step and is gated behind the 15-min wait.

## Denylist / revocation (related)

Revocation is independent of key rotation: `POST /v1/auth/revoke` adds a token's
`jti` to the replicated **bloom denylist** (`GET /v1/auth/denylist`), which the
gateway polls every `DENYLIST_POLL` (5 s in E2E). A revoked token is rejected at
the edge within one poll (≤ 30 s SLO). If `AuthRevocationLagCritical` fires, the
gateway's denylist poll is wedged or identity-auth's denylist endpoint is
unreachable — restart the poll loop / restore the endpoint; signature
verification is unaffected meanwhile.

## Identity-auth outage (D4 invariant)

If identity-auth is **down**: **already-issued tokens keep verifying at the
edge** (cached JWKS, no hot-path call) — authed traffic error rate must stay
flat (dashboard p5). Only **new** logins/refreshes and **new** revocations fail.
Do **not** fail authed traffic; page Identity & Trust to restore identity-auth,
but checkout SLOs are not affected (that is the point of D4).
