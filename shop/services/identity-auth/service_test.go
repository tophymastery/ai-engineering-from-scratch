package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/edgeauth"
	"github.com/shop-platform/shop/libs/logging"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	st, err := openStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.close() })
	km, err := newKeyManager()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	return &server{
		store: st, keys: km, deny: newDenylist(),
		log:   logging.New(logging.Config{Service: "identity-auth-test", Out: &bytes.Buffer{}}),
		iss:   "shop-identity", admin: true,
	}
}

func (s *server) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/v1/auth/register", s.only(http.MethodPost, s.handleRegister))
	m.HandleFunc("/v1/auth/login", s.only(http.MethodPost, s.handleLogin))
	m.HandleFunc("/v1/auth/refresh", s.only(http.MethodPost, s.handleRefresh))
	m.HandleFunc("/v1/auth/revoke", s.only(http.MethodPost, s.handleRevoke))
	m.HandleFunc("/v1/auth/denylist", s.only(http.MethodGet, s.handleDenylist))
	m.HandleFunc("/.well-known/jwks.json", s.handleJWKS)
	m.HandleFunc("/v1/auth/keys:rotate", s.only(http.MethodPost, s.handleKeyRotate))
	m.HandleFunc("/v1/auth/keys:retire", s.only(http.MethodPost, s.handleKeyRetire))
	return m
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestRegisterLoginFlow(t *testing.T) {
	s := newTestServer(t)
	h := s.mux()

	code, body := do(t, h, "POST", "/v1/auth/register", `{"email":"a@b.com","password":"hunter2pass"}`)
	if code != 201 {
		t.Fatalf("register: %d %v", code, body)
	}
	if body["role"] != "customer" || body["email"] != "a@b.com" {
		t.Fatalf("register body: %v", body)
	}

	// Duplicate email → 409 AUTH_EMAIL_TAKEN.
	code, body = do(t, h, "POST", "/v1/auth/register", `{"email":"A@b.com","password":"hunter2pass"}`)
	if code != 409 || body["error"].(map[string]any)["code"] != "AUTH_EMAIL_TAKEN" {
		t.Fatalf("dup register: %d %v", code, body)
	}

	// Wrong password → 401 AUTH_INVALID_CREDENTIALS (no enumeration).
	code, body = do(t, h, "POST", "/v1/auth/login", `{"email":"a@b.com","password":"wrong"}`)
	if code != 401 || body["error"].(map[string]any)["code"] != "AUTH_INVALID_CREDENTIALS" {
		t.Fatalf("bad login: %d %v", code, body)
	}
	// Unknown user → same code.
	code, body = do(t, h, "POST", "/v1/auth/login", `{"email":"nobody@b.com","password":"whatever0"}`)
	if code != 401 || body["error"].(map[string]any)["code"] != "AUTH_INVALID_CREDENTIALS" {
		t.Fatalf("unknown login: %d %v", code, body)
	}

	// Good login → tokens.
	code, body = do(t, h, "POST", "/v1/auth/login", `{"email":"a@b.com","password":"hunter2pass"}`)
	if code != 200 {
		t.Fatalf("login: %d %v", code, body)
	}
	if body["token_type"] != "Bearer" || body["expires_in"].(float64) != 900 {
		t.Fatalf("login token fields: %v", body)
	}
	access, _ := body["access_token"].(string)
	if access == "" {
		t.Fatal("no access_token")
	}
	// The minted access token verifies against the service's own JWKS.
	claims, err := edgeauth.Verify(access, s.keys.lookup, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("minted token failed verify: %v", err)
	}
	if claims.Role != "customer" || claims.Sub == "" || claims.JTI == "" {
		t.Fatalf("claims: %+v", claims)
	}
}

func TestRefreshRotatesAndRevokesOld(t *testing.T) {
	s := newTestServer(t)
	h := s.mux()
	do(t, h, "POST", "/v1/auth/register", `{"email":"r@b.com","password":"hunter2pass"}`)
	_, login := do(t, h, "POST", "/v1/auth/login", `{"email":"r@b.com","password":"hunter2pass"}`)
	oldRefresh := login["refresh_token"].(string)
	oldAccess := login["access_token"].(string)
	oldClaims, _ := edgeauth.Verify(oldAccess, s.keys.lookup, time.Now(), time.Minute)

	code, refr := do(t, h, "POST", "/v1/auth/refresh", `{"refresh_token":"`+oldRefresh+`"}`)
	if code != 200 {
		t.Fatalf("refresh: %d %v", code, refr)
	}
	newRefresh := refr["refresh_token"].(string)
	if newRefresh == oldRefresh {
		t.Fatal("refresh token was not rotated")
	}
	// Old access token's jti must now be on the denylist (rotation kills it).
	if !mustBloom(t, s.deny.snapshot()).Test(oldClaims.JTI) {
		t.Fatal("prior access jti not revoked on rotation")
	}
	// Reusing the OLD refresh token → 401 (already rotated).
	code, body := do(t, h, "POST", "/v1/auth/refresh", `{"refresh_token":"`+oldRefresh+`"}`)
	if code != 401 || body["error"].(map[string]any)["code"] != "AUTH_SESSION_INVALID" {
		t.Fatalf("reused refresh should 401: %d %v", code, body)
	}
	// The NEW refresh token still works.
	code, _ = do(t, h, "POST", "/v1/auth/refresh", `{"refresh_token":"`+newRefresh+`"}`)
	if code != 200 {
		t.Fatalf("new refresh should work: %d", code)
	}
}

func TestRevokeAddsJTIToDenylist(t *testing.T) {
	s := newTestServer(t)
	h := s.mux()
	do(t, h, "POST", "/v1/auth/register", `{"email":"v@b.com","password":"hunter2pass"}`)
	_, login := do(t, h, "POST", "/v1/auth/login", `{"email":"v@b.com","password":"hunter2pass"}`)
	access := login["access_token"].(string)
	refresh := login["refresh_token"].(string)
	claims, _ := edgeauth.Verify(access, s.keys.lookup, time.Now(), time.Minute)

	if mustBloom(t, s.deny.snapshot()).Test(claims.JTI) {
		t.Fatal("jti revoked before revoke call")
	}
	code, body := do(t, h, "POST", "/v1/auth/revoke", `{"refresh_token":"`+refresh+`"}`)
	if code != 200 || body["revoked"] != true {
		t.Fatalf("revoke: %d %v", code, body)
	}
	if !mustBloom(t, s.deny.snapshot()).Test(claims.JTI) {
		t.Fatal("jti not on denylist after revoke")
	}
}

// TestKeyRotationRunbook rehearses docs/runbooks/key-rotation.md: add key B →
// new tokens signed with B → tokens signed with A STILL verify → retire A.
func TestKeyRotationRunbook(t *testing.T) {
	s := newTestServer(t)
	h := s.mux()
	do(t, h, "POST", "/v1/auth/register", `{"email":"k@b.com","password":"hunter2pass"}`)

	// Token under key A.
	_, l1 := do(t, h, "POST", "/v1/auth/login", `{"email":"k@b.com","password":"hunter2pass"}`)
	tokA := l1["access_token"].(string)
	kidA := l1["kid"].(string)

	// Rotate → key B is primary, JWKS now advertises both.
	code, rot := do(t, h, "POST", "/v1/auth/keys:rotate", `{}`)
	if code != 200 {
		t.Fatalf("rotate: %d %v", code, rot)
	}
	kidB := rot["primary_kid"].(string)
	if kidB == kidA {
		t.Fatal("rotate did not change primary kid")
	}
	jwks := parseJWKS(t, h)
	if _, ok := jwks[kidA]; !ok {
		t.Fatal("JWKS dropped key A too early")
	}
	if _, ok := jwks[kidB]; !ok {
		t.Fatal("JWKS missing new key B")
	}

	// New token signed with B.
	_, l2 := do(t, h, "POST", "/v1/auth/login", `{"email":"k@b.com","password":"hunter2pass"}`)
	tokB := l2["access_token"].(string)
	if l2["kid"].(string) != kidB {
		t.Fatal("new token not signed with B")
	}

	// Both A and B verify against the current JWKS (overlap window).
	lookup := jwksLookup(jwks)
	if _, err := edgeauth.Verify(tokA, lookup, time.Now(), time.Minute); err != nil {
		t.Fatalf("old key-A token stopped verifying during overlap: %v", err)
	}
	if _, err := edgeauth.Verify(tokB, lookup, time.Now(), time.Minute); err != nil {
		t.Fatalf("new key-B token failed: %v", err)
	}

	// Retire A. Now only B remains; A-signed tokens no longer verify.
	code, ret := do(t, h, "POST", "/v1/auth/keys:retire", `{}`)
	if code != 200 {
		t.Fatalf("retire: %d %v", code, ret)
	}
	if ret["retired_kid"].(string) != kidA {
		t.Fatalf("retired wrong kid: %v", ret)
	}
	jwks2 := parseJWKS(t, h)
	if _, ok := jwks2[kidA]; ok {
		t.Fatal("key A still in JWKS after retire")
	}
	if _, err := edgeauth.Verify(tokA, jwksLookup(jwks2), time.Now(), time.Minute); err == nil {
		t.Fatal("key-A token verified after A retired")
	}
	if _, err := edgeauth.Verify(tokB, jwksLookup(jwks2), time.Now(), time.Minute); err != nil {
		t.Fatalf("key-B token failed after retire: %v", err)
	}
}

func TestConcurrentLogins(t *testing.T) {
	s := newTestServer(t)
	h := s.mux()
	do(t, h, "POST", "/v1/auth/register", `{"email":"c@b.com","password":"hunter2pass"}`)
	const N = 20
	errc := make(chan int, N)
	for i := 0; i < N; i++ {
		go func() {
			code, _ := do(t, h, "POST", "/v1/auth/login", `{"email":"c@b.com","password":"hunter2pass"}`)
			errc <- code
		}()
	}
	for i := 0; i < N; i++ {
		if code := <-errc; code != 200 {
			t.Fatalf("concurrent login failed: %d", code)
		}
	}
}

// helpers

func mustBloom(t *testing.T, snap edgeauth.BloomSnapshot) *edgeauth.Bloom {
	t.Helper()
	b, err := edgeauth.BloomFromSnapshot(snap)
	if err != nil {
		t.Fatalf("bloom from snapshot: %v", err)
	}
	return b
}

func parseJWKS(t *testing.T, h http.Handler) map[string]*ecdsa.PublicKey {
	t.Helper()
	req := httptest.NewRequest("GET", "/.well-known/jwks.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	m, err := edgeauth.ParseJWKS(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("parse jwks: %v", err)
	}
	return m
}

func jwksLookup(jwks map[string]*ecdsa.PublicKey) edgeauth.KeyLookup {
	return func(kid string) (*ecdsa.PublicKey, bool) {
		v, ok := jwks[kid]
		return v, ok
	}
}
