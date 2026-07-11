package edgeauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"testing"
	"time"
)

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return k
}

func lookupOf(pub *ecdsa.PublicKey, kid string) KeyLookup {
	return func(k string) (*ecdsa.PublicKey, bool) {
		if k == kid {
			return pub, true
		}
		return nil, false
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	priv := mustKey(t)
	kid := ThumbprintKID(&priv.PublicKey)
	now := time.Unix(1_800_000_000, 0)
	c := Claims{Iss: "identity-auth", Sub: "usr_abc", Role: "customer", JTI: "jti_1",
		Iat: now.Unix(), Nbf: now.Unix(), Exp: now.Add(15 * time.Minute).Unix()}
	tok, err := Sign(priv, kid, c)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := Verify(tok, lookupOf(&priv.PublicKey, kid), now, 0)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Sub != "usr_abc" || got.Role != "customer" || got.JTI != "jti_1" {
		t.Fatalf("claims mismatch: %+v", got)
	}
	if kid2, ok := PeekKID(tok); !ok || kid2 != kid {
		t.Fatalf("PeekKID = %q,%v want %q", kid2, ok, kid)
	}
}

func TestExpiredAndNotYetValidRejected(t *testing.T) {
	priv := mustKey(t)
	kid := ThumbprintKID(&priv.PublicKey)
	now := time.Unix(1_800_000_000, 0)
	lk := lookupOf(&priv.PublicKey, kid)

	expired := Claims{Sub: "u", JTI: "j", Iat: now.Add(-time.Hour).Unix(), Nbf: now.Add(-time.Hour).Unix(), Exp: now.Add(-time.Minute).Unix()}
	tok, _ := Sign(priv, kid, expired)
	if _, err := Verify(tok, lk, now, 0); err == nil {
		t.Fatal("expired token accepted")
	}
	future := Claims{Sub: "u", JTI: "j", Iat: now.Add(time.Hour).Unix(), Nbf: now.Add(time.Hour).Unix(), Exp: now.Add(2 * time.Hour).Unix()}
	tok2, _ := Sign(priv, kid, future)
	if _, err := Verify(tok2, lk, now, 0); err == nil {
		t.Fatal("not-yet-valid token accepted")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	priv := mustKey(t)
	other := mustKey(t)
	kid := ThumbprintKID(&priv.PublicKey)
	now := time.Unix(1_800_000_000, 0)
	tok, _ := Sign(priv, kid, Claims{Sub: "u", JTI: "j", Exp: now.Add(time.Minute).Unix()})
	// Present the WRONG public key under the same kid → signature must fail.
	if _, err := Verify(tok, lookupOf(&other.PublicKey, kid), now, 0); err == nil {
		t.Fatal("token verified against wrong key")
	}
}

// TestForgedTamperedExpired_1000 is the property-style criterion: over N=1000
// random mutations (bit-flips, expiry, unknown kid, alg swap, truncation) NOT A
// SINGLE forged/tampered/expired token may verify. Rejection rate must be 100%.
func TestForgedTamperedExpired_1000(t *testing.T) {
	priv := mustKey(t)
	kid := ThumbprintKID(&priv.PublicKey)
	lk := lookupOf(&priv.PublicKey, kid)
	now := time.Unix(1_800_000_000, 0)
	base := Claims{Iss: "identity-auth", Sub: "usr_v", Role: "customer", JTI: "jti_v",
		Iat: now.Unix(), Nbf: now.Unix(), Exp: now.Add(15 * time.Minute).Unix()}
	good, err := Sign(priv, kid, base)
	if err != nil {
		t.Fatal(err)
	}
	// sanity: the pristine token verifies.
	if _, err := Verify(good, lk, now, 0); err != nil {
		t.Fatalf("pristine token failed: %v", err)
	}

	rng := mrand.New(mrand.NewSource(42))
	const N = 1000
	accepted := 0
	for i := 0; i < N; i++ {
		var mutant string
		switch i % 5 {
		case 0: // random byte flip somewhere in the token
			bs := []byte(good)
			pos := rng.Intn(len(bs))
			bs[pos] ^= byte(1 + rng.Intn(255))
			mutant = string(bs)
		case 1: // valid signature but expired payload (re-signed)
			c := base
			c.Exp = now.Add(-time.Duration(1+rng.Intn(1000)) * time.Second).Unix()
			c.Nbf = c.Exp - 900
			mutant, _ = Sign(priv, kid, c)
		case 2: // signed by an attacker key, same kid claimed
			atk := mustKey(t)
			mutant, _ = Sign(atk, kid, base)
		case 3: // unknown kid (JWKS refresh would also miss)
			mutant, _ = Sign(priv, fmt.Sprintf("k_forged_%d", i), base)
		case 4: // truncate / structurally corrupt
			cut := 1 + rng.Intn(len(good)-1)
			mutant = good[:cut]
		}
		if mutant == good {
			continue // skip a no-op mutation
		}
		if _, err := Verify(mutant, lk, now, 0); err == nil {
			accepted++
			t.Errorf("mutation %d (kind %d) ACCEPTED a forged/expired token", i, i%5)
		}
	}
	if accepted != 0 {
		t.Fatalf("rejection rate %.2f%% — want 100%% (%d/%d forged tokens accepted)",
			100*float64(N-accepted)/float64(N), accepted, N)
	}
	t.Logf("forged/tampered/expired rejection: %d/%d = 100%%", N, N)
}

// TestAlgNoneRejected guards the classic JOSE downgrade: an attacker sets
// alg:none (or HS256) and drops the signature.
func TestAlgNoneRejected(t *testing.T) {
	priv := mustKey(t)
	kid := ThumbprintKID(&priv.PublicKey)
	now := time.Unix(1_800_000_000, 0)
	// Hand-craft an alg:none token: header.payload. with empty sig.
	hdr := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + kid + `"}`))
	pl := b64.EncodeToString([]byte(`{"sub":"usr_attacker","exp":9999999999}`))
	none := hdr + "." + pl + "."
	if _, err := Verify(none, lookupOf(&priv.PublicKey, kid), now, 0); err == nil {
		t.Fatal("alg:none token accepted")
	}
}

func TestJWKSRoundTrip(t *testing.T) {
	priv := mustKey(t)
	jwk := PublicJWK(&priv.PublicKey)
	if jwk.Kid != ThumbprintKID(&priv.PublicKey) {
		t.Fatal("kid mismatch")
	}
	pub, err := jwk.PublicKey()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pub.X.Cmp(priv.PublicKey.X) != 0 || pub.Y.Cmp(priv.PublicKey.Y) != 0 {
		t.Fatal("public key coords changed through JWK round-trip")
	}
	doc := JWKS{Keys: []JWK{jwk}}
	blob := mustJSON(t, doc)
	m, err := ParseJWKS(blob)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}
	if _, ok := m[jwk.Kid]; !ok {
		t.Fatal("kid missing after ParseJWKS")
	}
}

func TestBloomNoFalseNegatives(t *testing.T) {
	b := NewBloom(0, 0)
	revoked := make([]string, 5000)
	for i := range revoked {
		revoked[i] = fmt.Sprintf("jti_%d", i)
		b.Add(revoked[i])
	}
	for _, j := range revoked {
		if !b.Test(j) {
			t.Fatalf("false negative: revoked %q not detected", j)
		}
	}
	// Snapshot → reconstruct → identical membership (issuer/verifier parity).
	snap := b.Snapshot(7)
	vb, err := BloomFromSnapshot(snap)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	for _, j := range revoked {
		if !vb.Test(j) {
			t.Fatalf("reconstructed filter lost revocation %q", j)
		}
	}
	// False-positive sanity: unrelated jtis rarely collide at this fill level.
	fp := 0
	for i := 0; i < 10000; i++ {
		if vb.Test(fmt.Sprintf("live_%d", i)) {
			fp++
		}
	}
	if rate := float64(fp) / 10000; rate > 0.02 {
		t.Fatalf("false-positive rate %.4f too high (want < 0.02)", rate)
	}
	t.Logf("bloom: 5000 revoked, 0 false negatives, FP=%d/10000", fp)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
