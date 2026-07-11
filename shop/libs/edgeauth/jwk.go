package edgeauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
)

// JWK is a single EC P-256 public key in the JWKS (RFC 7517 / 7518 §6.2). Only
// the fields a verifier needs are modelled.
type JWK struct {
	Kty string `json:"kty"` // "EC"
	Crv string `json:"crv"` // "P-256"
	Kid string `json:"kid"`
	Use string `json:"use"` // "sig"
	Alg string `json:"alg"` // "ES256"
	X   string `json:"x"`   // base64url big-endian, 32 bytes
	Y   string `json:"y"`
}

// JWKS is the /.well-known/jwks.json document. D4 supports 2 keys at once so a
// rotation can publish the new key before signing with it and retire the old
// one after all its tokens expire.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// ThumbprintKID derives a stable, collision-resistant kid from the public key
// itself (a truncated SHA-256 over the curve+coords, RFC 7638 in spirit). Same
// key ⇒ same kid on issuer and verifier, with no coordination.
func ThumbprintKID(pub *ecdsa.PublicKey) string {
	xb, yb := coord(pub.X), coord(pub.Y)
	sum := sha256.Sum256(append(append([]byte("P-256|"), xb...), yb...))
	return "k_" + b64.EncodeToString(sum[:12])
}

func coord(n *big.Int) []byte {
	b := make([]byte, 32) // P-256 field element is exactly 32 bytes
	n.FillBytes(b)
	return b
}

// PublicJWK encodes an ECDSA P-256 public key as a JWK with a thumbprint kid.
func PublicJWK(pub *ecdsa.PublicKey) JWK {
	return JWK{
		Kty: "EC", Crv: "P-256", Use: "sig", Alg: "ES256",
		Kid: ThumbprintKID(pub),
		X:   b64.EncodeToString(coord(pub.X)),
		Y:   b64.EncodeToString(coord(pub.Y)),
	}
}

// PublicKey decodes a JWK back into an *ecdsa.PublicKey (P-256 only).
func (j JWK) PublicKey() (*ecdsa.PublicKey, error) {
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, fmt.Errorf("edgeauth: unsupported JWK kty=%q crv=%q", j.Kty, j.Crv)
	}
	xb, err := b64.DecodeString(j.X)
	if err != nil {
		return nil, fmt.Errorf("edgeauth: JWK x: %w", err)
	}
	yb, err := b64.DecodeString(j.Y)
	if err != nil {
		return nil, fmt.Errorf("edgeauth: JWK y: %w", err)
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xb),
		Y:     new(big.Int).SetBytes(yb),
	}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, fmt.Errorf("edgeauth: JWK point not on P-256")
	}
	return pub, nil
}

// ParseJWKS decodes a JWKS document into a kid→publickey map, skipping keys that
// fail to decode (a bad key must not poison the others). It returns an error
// only if the document itself is unparseable.
func ParseJWKS(b []byte) (map[string]*ecdsa.PublicKey, error) {
	var doc JWKS
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("edgeauth: parse JWKS: %w", err)
	}
	out := make(map[string]*ecdsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if pub, err := k.PublicKey(); err == nil && k.Kid != "" {
			out[k.Kid] = pub
		}
	}
	return out, nil
}
