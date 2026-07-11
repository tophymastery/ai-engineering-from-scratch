package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// keys.go — the HMAC signing-key ring for quotes, mirroring V-T1's ES256
// keyManager (services/identity-auth/keys.go) but with a SYMMETRIC primitive.
// D10 says quotes are "signed (HMAC over quote body + expiry) so checkout can
// verify integrity". The rotation contract is identical to V-T1's:
//
//   - a single ACTIVE signing key (`primary`) signs new quotes;
//   - VERIFICATION accepts any key in the set, so a quote signed by the outgoing
//     key keeps verifying through the overlap window (a quote's max life = its
//     10-min TTL, so the outgoing key must stay ≥10 min after it stops signing);
//   - the set holds at most 2 keys ⇒ always exactly one overlap window;
//   - rotate() = add + make primary + cap to 2; retire() drops the oldest and
//     refuses to retire the primary or the last key.
//
// rotate()/retire() are the two runbook steps (docs/runbooks/quote-key-rotation.md),
// rehearsed by TestKeyRotationRunbook (sign.go's verify path) and
// tools/rotate-quote-keys-demo.sh.
type keyEntry struct {
	kid     string
	secret  []byte // 32-byte HMAC-SHA256 key
	created time.Time
}

type keyManager struct {
	mu      sync.RWMutex
	keys    []*keyEntry // oldest → newest
	primary string      // kid used for signing
	clock   Clock
}

// kidFromSecret derives a stable, collision-resistant kid from the key material
// itself (truncated SHA-256), so signer and any verifier agree on the kid with
// no coordination — the HMAC analogue of V-T1's ThumbprintKID over the public
// key. The kid is NOT the secret (it is a one-way digest) so exposing it in the
// quote leaks nothing.
func kidFromSecret(secret []byte) string {
	sum := sha256.Sum256(append([]byte("pricing-hmac|"), secret...))
	return "hk_" + hex.EncodeToString(sum[:9])
}

// newKeyManager builds the ring with one freshly-generated active key (parity
// with V-T1: keys are ephemeral + in-process; rotation happens at runtime via
// the admin endpoints). A production deployment would load the seed HMAC secrets
// from the per-cell secret store keyed by kid — see the runbook.
func newKeyManager(clock Clock) (*keyManager, error) {
	km := &keyManager{clock: clock}
	if _, err := km.add(); err != nil {
		return nil, err
	}
	return km, nil
}

// add generates a new random HMAC key, appends it, and makes it the primary
// signer.
func (km *keyManager) add() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	kid := kidFromSecret(secret)
	km.mu.Lock()
	defer km.mu.Unlock()
	km.keys = append(km.keys, &keyEntry{kid: kid, secret: secret, created: km.now()})
	km.primary = kid
	return kid, nil
}

// rotate is runbook step 1: introduce a new key B, make it the primary signer,
// keep the outgoing key A in the set (still verifies in-flight quotes). Caps the
// set at 2 keys (one overlap window).
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

// retire is runbook step 2: drop the oldest (outgoing) key once every quote it
// signed has expired (≥10-min TTL). Refuses to retire the primary or the last
// key — the only irreversible step, gated behind the operator's wait.
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

// signingKey returns the current primary (kid, secret) for signing a new quote.
func (km *keyManager) signingKey() (string, []byte, error) {
	km.mu.RLock()
	defer km.mu.RUnlock()
	for _, e := range km.keys {
		if e.kid == km.primary {
			return e.kid, e.secret, nil
		}
	}
	return "", nil, fmt.Errorf("no primary key")
}

// lookup resolves a kid to its secret for verification. ok=false for an unknown
// or retired kid — which is exactly how a quote signed by a retired key (or a
// forged kid) is rejected.
func (km *keyManager) lookup(kid string) ([]byte, bool) {
	km.mu.RLock()
	defer km.mu.RUnlock()
	for _, e := range km.keys {
		if e.kid == kid {
			return e.secret, true
		}
	}
	return nil, false
}

// primaryKID / kids expose ring state for /healthz and the rotation rehearsal.
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

func (km *keyManager) now() time.Time {
	if km.clock != nil {
		return km.clock.Now()
	}
	return time.Now().UTC()
}
