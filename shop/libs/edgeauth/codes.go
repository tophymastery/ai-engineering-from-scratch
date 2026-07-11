package edgeauth

import shoperr "github.com/shop-platform/shop/libs/errors"

// Edge auth error codes (02 §2 registry). Registered here so every importer —
// gateway verifier and identity-auth issuer alike — maps them to the same HTTP
// status. A revoked or malformed token is a 401 (re-authenticate), never a 403.
var (
	// CodeTokenInvalid: the bearer token is malformed, has a bad signature, an
	// unknown kid, or is expired/not-yet-valid. The client must re-authenticate.
	CodeTokenInvalid = shoperr.Register("AUTH_TOKEN_INVALID", 401, false,
		"The access token is missing, malformed, expired, or has an invalid signature.")
	// CodeTokenRevoked: the token's jti is on the replicated bloom denylist
	// (D4). Distinct from INVALID so dashboards can separate revocations from
	// bad/forged tokens.
	CodeTokenRevoked = shoperr.Register("AUTH_TOKEN_REVOKED", 401, false,
		"The access token has been revoked.")
)
