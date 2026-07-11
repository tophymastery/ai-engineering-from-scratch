package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/edgeauth"
)

// keyEntry is one ES256 signing key in the rotation set.
type keyEntry struct {
	kid     string
	priv    *ecdsa.PrivateKey
	created time.Time
}

// keyManager holds the active ES256 keys (D4: JWKS supports 2 keys for
// rotation). Signing always uses `primary`; verification accepts any key in the
// set, so a token minted by the outgoing key keeps verifying until it expires
// (≤15 min) even after a new key becomes primary. rotate()/retire() are the two
// runbook steps, rehearsed by tools/rotate-keys-demo.sh.
type keyManager struct {
	mu      sync.RWMutex
	keys    []*keyEntry // oldest → newest
	primary string      // kid used for signing
}

func newKeyManager() (*keyManager, error) {
	km := &keyManager{}
	if _, err := km.add(); err != nil {
		return nil, err
	}
	return km, nil
}

// add generates a new key, appends it, and makes it the primary signer.
func (km *keyManager) add() (string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	kid := edgeauth.ThumbprintKID(&priv.PublicKey)
	km.mu.Lock()
	defer km.mu.Unlock()
	km.keys = append(km.keys, &keyEntry{kid: kid, priv: priv, created: time.Now()})
	km.primary = kid
	return kid, nil
}

// rotate is runbook step 1: introduce a new key B, publish it in JWKS, and sign
// new tokens with it. Old key A stays in the set (still verifies). If the set
// would exceed 2 keys the oldest is dropped, keeping JWKS at the D4 max of 2.
func (km *keyManager) rotate() (string, error) {
	kid, err := km.add()
	if err != nil {
		return "", err
	}
	km.mu.Lock()
	if len(km.keys) > 2 {
		km.keys = km.keys[len(km.keys)-2:]
	}
	km.mu.Unlock()
	return kid, nil
}

// retire is runbook step 2: drop the oldest (outgoing) key once all tokens it
// signed have expired. Refuses to retire the primary or the last key.
func (km *keyManager) retire() (string, error) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if len(km.keys) <= 1 {
		return "", fmt.Errorf("cannot retire the only key")
	}
	oldest := km.keys[0]
	if oldest.kid == km.primary {
		return "", fmt.Errorf("cannot retire the primary signing key")
	}
	km.keys = km.keys[1:]
	return oldest.kid, nil
}

// sign mints an ES256 JWT for the given claims using the current primary key,
// stamping its kid in the header so the verifier picks the right JWKS entry.
func (km *keyManager) sign(c edgeauth.Claims) (string, string, error) {
	km.mu.RLock()
	defer km.mu.RUnlock()
	for _, e := range km.keys {
		if e.kid == km.primary {
			tok, err := edgeauth.Sign(e.priv, e.kid, c)
			return tok, e.kid, err
		}
	}
	return "", "", fmt.Errorf("no primary key")
}

// lookup returns the public key for a kid (verifier side / self-tests).
func (km *keyManager) lookup(kid string) (*ecdsa.PublicKey, bool) {
	km.mu.RLock()
	defer km.mu.RUnlock()
	for _, e := range km.keys {
		if e.kid == kid {
			return &e.priv.PublicKey, true
		}
	}
	return nil, false
}

// jwks renders the public JWKS document served at /.well-known/jwks.json.
func (km *keyManager) jwks() edgeauth.JWKS {
	km.mu.RLock()
	defer km.mu.RUnlock()
	doc := edgeauth.JWKS{Keys: make([]edgeauth.JWK, 0, len(km.keys))}
	for _, e := range km.keys {
		doc.Keys = append(doc.Keys, edgeauth.PublicJWK(&e.priv.PublicKey))
	}
	return doc
}

func (km *keyManager) primaryKID() string {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.primary
}

func (km *keyManager) kids() []string {
	km.mu.RLock()
	defer km.mu.RUnlock()
	out := make([]string, 0, len(km.keys))
	for _, e := range km.keys {
		out = append(out, e.kid)
	}
	return out
}
