// Package edgeauth is the shared, dependency-light implementation of the D4
// "stateless auth at the edge" primitives, imported by BOTH the token ISSUER
// (services/identity-auth) and the token VERIFIER (gateway). Putting the crypto
// in one place is load-bearing: the gateway must verify byte-for-byte what
// identity-auth signed, and must reconstruct the exact bloom denylist
// identity-auth published — any drift between two hand-rolled copies would be a
// silent auth bug. So the ES256 JWT codec, the EC P-256 JWK encoding, and the
// bloom-filter denylist all live here and are unit-tested here.
//
// Zero third-party deps (std-lib crypto only): ES256 = ecdsa over P-256 with a
// SHA-256 digest and a raw 64-byte r||s signature (RFC 7518 §3.4), base64url
// (no padding) segments (RFC 7515). The bloom denylist is a plain bitset with
// double-hashing (Kirsch-Mitzenmacher) so a snapshot is a base64 bitset plus
// (k, m, version) — small enough to poll every few seconds (D4: ≤30 s
// revocation lag, no introspection on the hot path).
package edgeauth
