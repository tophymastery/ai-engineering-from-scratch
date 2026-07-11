package edgeauth

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

// b64 is base64url without padding (RFC 7515 §2 / RFC 4648 §5), used for
// ENCODING our own segments.
var b64 = base64.RawURLEncoding

// b64strict additionally rejects non-canonical trailing bits on DECODE. Without
// it, flipping the unused bits of a segment's final base64 char decodes to the
// SAME bytes, so a tampered signature could still verify — a real forgery hole
// the 100%-rejection property would otherwise miss.
var b64strict = base64.RawURLEncoding.Strict()

// Claims is the compact D4 access-token payload. 15-minute lifetime is enforced
// by the issuer (identity-auth); the verifier only checks exp/nbf are consistent
// with `now`. `jti` is the denylist key (revocation), `sub` the prefixed user id
// (usr_…), `role` the coarse authz role passed upstream as X-Auth-Role.
type Claims struct {
	Iss  string `json:"iss"`
	Sub  string `json:"sub"`
	Role string `json:"role"`
	JTI  string `json:"jti"`
	Sid  string `json:"sid,omitempty"` // session id (refresh-token family) — ops only
	Iat  int64  `json:"iat"`
	Nbf  int64  `json:"nbf"`
	Exp  int64  `json:"exp"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Sign builds an ES256 JWT: base64url(header).base64url(claims).base64url(r||s).
// The signature is a raw 64-byte r||s (RFC 7518 §3.4) — NOT ASN.1 DER — which is
// what every JOSE verifier expects for ES256.
func Sign(priv *ecdsa.PrivateKey, kid string, c Claims) (string, error) {
	if priv == nil {
		return "", fmt.Errorf("edgeauth: nil signing key")
	}
	hb, err := json.Marshal(jwtHeader{Alg: "ES256", Typ: "JWT", Kid: kid})
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := b64.EncodeToString(hb) + "." + b64.EncodeToString(pb)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32]) // fixed-width, left-zero-padded per RFC 7518
	s.FillBytes(sig[32:])
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// KeyLookup returns the verifying public key for a kid. It reports ok=false for
// an unknown kid so the caller (gateway) can refresh JWKS and retry once.
type KeyLookup func(kid string) (*ecdsa.PublicKey, bool)

// PeekKID returns the `kid` header of a token without verifying anything. The
// gateway uses it to decide whether a JWKS refresh is worth attempting.
func PeekKID(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	hb, err := b64strict.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var h jwtHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return "", false
	}
	return h.Kid, h.Kid != ""
}

// Verify parses an ES256 JWT, verifies its signature against the key named by
// its kid (via lookup), and checks nbf/exp against now. On any failure it
// returns a registered *errors.Error (AUTH_TOKEN_INVALID) so the caller can
// serialise the 02 §2 envelope directly. Revocation (denylist) is checked
// separately by the caller — this function is pure signature+time validation.
//
// leeway absorbs small clock skew across cells (D4 tokens are signed per cell).
func Verify(token string, lookup KeyLookup, now time.Time, leeway time.Duration) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, shoperr.New(CodeTokenInvalid, "malformed token (want 3 segments)")
	}
	hb, err := b64strict.DecodeString(parts[0])
	if err != nil {
		return Claims{}, shoperr.New(CodeTokenInvalid, "undecodable header")
	}
	var h jwtHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return Claims{}, shoperr.New(CodeTokenInvalid, "unparseable header")
	}
	if h.Alg != "ES256" {
		// Reject anything but ES256 — never honour alg:none or an HMAC downgrade.
		return Claims{}, shoperr.New(CodeTokenInvalid, "unexpected alg "+h.Alg)
	}
	pub, ok := lookup(h.Kid)
	if !ok || pub == nil {
		return Claims{}, shoperr.New(CodeTokenInvalid, "unknown signing key (kid)")
	}
	pb, err := b64strict.DecodeString(parts[1])
	if err != nil {
		return Claims{}, shoperr.New(CodeTokenInvalid, "undecodable payload")
	}
	sig, err := b64strict.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return Claims{}, shoperr.New(CodeTokenInvalid, "bad signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return Claims{}, shoperr.New(CodeTokenInvalid, "signature verification failed")
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return Claims{}, shoperr.New(CodeTokenInvalid, "unparseable claims")
	}
	nowU := now.Unix()
	lw := int64(leeway / time.Second)
	if c.Exp != 0 && nowU > c.Exp+lw {
		return Claims{}, shoperr.New(CodeTokenInvalid, "token expired")
	}
	if c.Nbf != 0 && nowU+lw < c.Nbf {
		return Claims{}, shoperr.New(CodeTokenInvalid, "token not yet valid")
	}
	return c, nil
}
