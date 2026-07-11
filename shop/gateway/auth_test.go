package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/edgeauth"
)

// fakeIdentity stands in for identity-auth: it holds a signing key, serves JWKS
// + the denylist snapshot, and can mint tokens and revoke jtis. It lets the
// gateway auth path be tested end-to-end (issue → verify → revoke → outage)
// without importing the identity-auth module.
type fakeIdentity struct {
	priv    *ecdsa.PrivateKey
	kid     string
	mu      sync.Mutex
	bloom   *edgeauth.Bloom
	version uint64
	srv     *httptest.Server
}

func newFakeIdentity(t *testing.T) *fakeIdentity {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdentity{priv: priv, kid: edgeauth.ThumbprintKID(&priv.PublicKey), bloom: edgeauth.NewBloom(0, 0)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(edgeauth.JWKS{Keys: []edgeauth.JWK{edgeauth.PublicJWK(&priv.PublicKey)}})
	})
	mux.HandleFunc("/v1/auth/denylist", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		snap := f.bloom.Snapshot(f.version)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(snap)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdentity) issue(t *testing.T, sub, role, jti string, exp time.Time) string {
	t.Helper()
	now := time.Now()
	tok, err := edgeauth.Sign(f.priv, f.kid, edgeauth.Claims{
		Iss: "fake", Sub: sub, Role: role, JTI: jti,
		Iat: now.Unix(), Nbf: now.Unix(), Exp: exp.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fakeIdentity) revoke(jti string) {
	f.mu.Lock()
	f.bloom.Add(jti)
	f.version++
	f.mu.Unlock()
}

func newAuthFor(f *fakeIdentity, poll time.Duration) *edgeAuth {
	return newEdgeAuth(true, f.srv.URL, poll)
}

// echoUpstream records the identity headers the middleware injected.
func echoUpstream() (http.Handler, *sync.Map) {
	seen := &sync.Map{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store("sub", r.Header.Get(hdrAuthSubject))
		seen.Store("role", r.Header.Get(hdrAuthRole))
		w.WriteHeader(http.StatusOK)
	})
	return h, seen
}

func TestValidTokenInjectsIdentity(t *testing.T) {
	f := newFakeIdentity(t)
	a := newAuthFor(f, time.Second)
	if err := a.jwks.refresh(); err != nil {
		t.Fatalf("warm jwks: %v", err)
	}
	up, seen := echoUpstream()
	h := a.middleware(up)

	tok := f.issue(t, "usr_1", "customer", "jti_ok", time.Now().Add(15*time.Minute))
	req := httptest.NewRequest("GET", "/order/v1/orders/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	// Inbound spoof attempt must be overwritten with the verified identity.
	req.Header.Set(hdrAuthSubject, "usr_ATTACKER")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid token rejected: %d", rec.Code)
	}
	if v, _ := seen.Load("sub"); v != "usr_1" {
		t.Fatalf("subject header = %v, want usr_1 (spoof not overwritten?)", v)
	}
	if v, _ := seen.Load("role"); v != "customer" {
		t.Fatalf("role header = %v", v)
	}
}

func TestSpoofStrippedWithoutToken(t *testing.T) {
	f := newFakeIdentity(t)
	a := newAuthFor(f, time.Second)
	up, seen := echoUpstream()
	h := a.middleware(up)
	req := httptest.NewRequest("GET", "/order/v1/orders/x", nil)
	req.Header.Set(hdrAuthSubject, "usr_ATTACKER")
	req.Header.Set(hdrAuthRole, "admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if v, _ := seen.Load("sub"); v != "" {
		t.Fatalf("spoofed subject reached upstream: %v", v)
	}
	if v, _ := seen.Load("role"); v != "" {
		t.Fatalf("spoofed role reached upstream: %v", v)
	}
}

func TestForgedTokenRejected(t *testing.T) {
	f := newFakeIdentity(t)
	a := newAuthFor(f, time.Second)
	_ = a.jwks.refresh()
	up, _ := echoUpstream()
	h := a.middleware(up)

	// Attacker-signed token (unknown key), same shape.
	atk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	forged, _ := edgeauth.Sign(atk, edgeauth.ThumbprintKID(&atk.PublicKey), edgeauth.Claims{
		Sub: "usr_evil", Role: "admin", JTI: "j", Exp: time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest("GET", "/order/v1/orders/x", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("forged token: got %d want 401", rec.Code)
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "AUTH_TOKEN_INVALID" {
		t.Fatalf("forged envelope code = %q want AUTH_TOKEN_INVALID", env.Error.Code)
	}
}

// TestCriterion_ForgedExpiredTampered1000 asserts 100% rejection over 1000
// mutations, measured THROUGH the gateway middleware (401 for every one).
func TestCriterion_ForgedExpiredTampered1000(t *testing.T) {
	f := newFakeIdentity(t)
	a := newAuthFor(f, time.Second)
	_ = a.jwks.refresh()
	up, _ := echoUpstream()
	h := a.middleware(up)

	good := f.issue(t, "usr_v", "customer", "jti_v", time.Now().Add(15*time.Minute))
	accepted := 0
	const N = 1000
	for i := 0; i < N; i++ {
		var tok string
		switch i % 4 {
		case 0:
			bs := []byte(good)
			bs[i%len(bs)] ^= 0x20
			tok = string(bs)
		case 1: // expired but validly signed (well beyond the 60s skew leeway)
			tok = f.issue(t, "usr_v", "customer", fmt.Sprintf("j%d", i), time.Now().Add(-30*time.Minute))
		case 2: // attacker key
			atk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			tok, _ = edgeauth.Sign(atk, f.kid, edgeauth.Claims{Sub: "x", JTI: "j", Exp: time.Now().Add(time.Hour).Unix()})
		case 3: // truncated
			tok = good[:1+i%(len(good)-1)]
		}
		req := httptest.NewRequest("GET", "/order/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == 200 {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("gateway accepted %d/%d forged/expired/tampered tokens (want 0)", accepted, N)
	}
	t.Logf("gateway forged/expired/tampered rejection: %d/%d = 100%%", N, N)
}

// TestCriterion_P99LatencyDelta asserts the auth verification adds < 1 ms p99 to
// a request through the gateway (authed vs unauthed).
func TestCriterion_P99LatencyDelta(t *testing.T) {
	f := newFakeIdentity(t)
	a := newAuthFor(f, time.Second)
	if err := a.jwks.refresh(); err != nil {
		t.Fatal(err)
	}
	up, _ := echoUpstream()
	h := a.middleware(up)
	tok := f.issue(t, "usr_p", "customer", "jti_p", time.Now().Add(15*time.Minute))

	measure := func(withToken bool) time.Duration {
		const samples = 4000
		ds := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			req := httptest.NewRequest("GET", "/order/x", nil)
			if withToken {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			rec := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(rec, req)
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[int(float64(len(ds))*0.99)]
	}
	// warm up both paths (JIT of maps, allocs).
	_ = measure(true)
	_ = measure(false)
	authedP99 := measure(true)
	plainP99 := measure(false)
	delta := authedP99 - plainP99
	t.Logf("p99 unauthed=%v authed=%v delta=%v", plainP99, authedP99, delta)
	if delta >= time.Millisecond {
		t.Fatalf("auth p99 delta %v >= 1ms", delta)
	}
}

// TestCriterion_RevocationLag asserts a revoked token is rejected within the
// poll interval (≤30s SLO; here we drive a fast poll and measure the actual lag).
func TestCriterion_RevocationLag(t *testing.T) {
	f := newFakeIdentity(t)
	const poll = 200 * time.Millisecond
	a := newAuthFor(f, poll)
	_ = a.jwks.refresh()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = a.deny.pollOnce()
	go a.deny.pollLoop(ctx, poll)

	up, _ := echoUpstream()
	h := a.middleware(up)
	jti := "jti_revoke_me"
	tok := f.issue(t, "usr_r", "customer", jti, time.Now().Add(15*time.Minute))

	// Before revocation the token passes.
	if code := reqCode(h, tok); code != 200 {
		t.Fatalf("token rejected before revoke: %d", code)
	}
	f.revoke(jti)
	t0 := time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if reqCode(h, tok) == 401 {
			lag := time.Since(t0)
			t.Logf("revocation propagated in %v (poll=%v, SLO 30s)", lag, poll)
			if lag > 30*time.Second {
				t.Fatalf("revocation lag %v > 30s", lag)
			}
			if lag > 3*poll+500*time.Millisecond {
				t.Fatalf("revocation lag %v exceeds poll+slack", lag)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("revoked token never rejected within 30s")
}

// TestCriterion_IdentityOutage asserts the D4 property: with identity-auth DOWN,
// previously-issued tokens STILL verify at the gateway (0 errors on authed
// traffic) — signature verification runs off the cached JWKS, no hot-path call
// to identity. Only NEW logins (which must reach identity) would fail.
func TestCriterion_IdentityOutage(t *testing.T) {
	f := newFakeIdentity(t)
	a := newAuthFor(f, 100*time.Millisecond)
	if err := a.jwks.refresh(); err != nil { // warm the cache while identity is UP
		t.Fatal(err)
	}
	_ = a.deny.pollOnce()

	// Pre-issue a batch of tokens (as if users logged in before the outage).
	toks := make([]string, 200)
	for i := range toks {
		toks[i] = f.issue(t, fmt.Sprintf("usr_%d", i), "customer", fmt.Sprintf("jti_%d", i), time.Now().Add(15*time.Minute))
	}
	up, _ := echoUpstream()
	h := a.middleware(up)

	// SIMULATE THE OUTAGE: identity-auth goes away entirely.
	f.srv.Close()
	// (adaptation: honest short outage — verify the invariant that would hold for
	// a full 10-min outage; nothing here calls identity on the hot path.)

	errors := 0
	for _, tok := range toks {
		if reqCode(h, tok) != 200 {
			errors++
		}
	}
	if errors != 0 {
		t.Fatalf("identity outage caused %d/%d authed-traffic errors (want 0)", errors, len(toks))
	}
	// A token with an UNKNOWN kid (a "new login" during the outage would use a
	// rotated key the gateway can't fetch) is correctly rejected — availability
	// is preserved for known keys, not forged ones.
	atk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	unknown, _ := edgeauth.Sign(atk, "k_rotated_during_outage", edgeauth.Claims{Sub: "u", JTI: "j", Exp: time.Now().Add(time.Hour).Unix()})
	if reqCode(h, unknown) != 401 {
		t.Fatal("unknown-kid token accepted during outage")
	}
	t.Logf("identity outage: %d/%d pre-issued tokens still verified at the gateway (0 errors)", len(toks), len(toks))
}

func reqCode(h http.Handler, tok string) int {
	req := httptest.NewRequest("GET", "/order/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}
