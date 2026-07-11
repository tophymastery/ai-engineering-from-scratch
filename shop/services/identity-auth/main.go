// Command identity-auth is the V-T1 accounts & sessions service (implements D4).
// It OWNS credentials and refresh sessions, and it MINTS the short-lived ES256
// access tokens the gateway verifies locally. It is deliberately OFF the request
// hot path: after login, a client's authenticated requests are verified at the
// edge from cached JWKS + a polled bloom denylist, never here (250k RPS ×
// introspection would make identity the global SPOF — the D4 rationale).
//
// Endpoints (02 §1 conventions, 02 §2 error envelope, libs/logging+otel+errors):
//
//	POST /v1/auth/register  email+password → user
//	POST /v1/auth/login     → {access_token (ES256, 15m, kid), refresh_token (opaque)}
//	POST /v1/auth/refresh   rotate the refresh token; revoke the prior access jti
//	POST /v1/auth/revoke    revoke a session → add its access jti to the denylist
//	GET  /v1/auth/denylist  bloom snapshot the gateway polls ≤30s (base64 + k/m/version)
//	GET  /.well-known/jwks.json  public keys (2 for rotation) — infra path, not in the contract
//	POST /v1/auth/keys:rotate | :retire  operational key-rotation (non-prod; runbook rehearsal)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shop-platform/shop/libs/edgeauth"
	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

// Domain error codes owned by this slice (edge codes come from libs/edgeauth).
var (
	codeInvalidCreds = shoperr.Register("AUTH_INVALID_CREDENTIALS", 401, false, "Email or password is incorrect.")
	codeEmailTaken   = shoperr.Register("AUTH_EMAIL_TAKEN", 409, false, "An account with this email already exists.")
	codeSessionInval = shoperr.Register("AUTH_SESSION_INVALID", 401, false, "The refresh token is invalid, expired, revoked, or already rotated.")
)

const (
	accessTTL  = 15 * time.Minute      // D4: 15-minute access tokens
	refreshTTL = 30 * 24 * time.Hour   // opaque refresh tokens live longer
)

type server struct {
	store *store
	keys  *keyManager
	deny  *denylist
	log   *logging.Logger
	iss   string
	admin bool // key-rotation endpoints enabled (non-prod)
}

func main() {
	port := envOr("PORT", "8101")
	name := envOr("SERVICE_NAME", "identity-auth")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	ctx := context.Background()
	st, err := openStore(ctx, envOr("IDENTITY_DB", ":memory:"))
	if err != nil {
		log.Fatalf("identity-auth: open store: %v", err)
	}
	km, err := newKeyManager()
	if err != nil {
		log.Fatalf("identity-auth: keygen: %v", err)
	}
	env := envOr("ENV", "dev")
	srv := &server{
		store: st,
		keys:  km,
		deny:  newDenylist(),
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: envOr("REGION", "local"), SampleRate: 1.0,
		}),
		iss:   envOr("TOKEN_ISS", "shop-identity"),
		admin: env != "prod", // rotation endpoints are ops-only, never in prod build path
	}
	_ = flags.FromEnv() // env-flag surface available; this service has no request flag gate

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/.well-known/jwks.json", srv.handleJWKS)
	mux.HandleFunc("/v1/auth/register", srv.only(http.MethodPost, srv.handleRegister))
	mux.HandleFunc("/v1/auth/login", srv.only(http.MethodPost, srv.handleLogin))
	mux.HandleFunc("/v1/auth/refresh", srv.only(http.MethodPost, srv.handleRefresh))
	mux.HandleFunc("/v1/auth/revoke", srv.only(http.MethodPost, srv.handleRevoke))
	mux.HandleFunc("/v1/auth/denylist", srv.only(http.MethodGet, srv.handleDenylist))
	mux.HandleFunc("/v1/auth/keys:rotate", srv.only(http.MethodPost, srv.handleKeyRotate))
	mux.HandleFunc("/v1/auth/keys:retire", srv.only(http.MethodPost, srv.handleKeyRetire))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("identity-auth %q on %s (primary_kid=%s env=%s admin=%v)", name, addr, km.primaryKID(), env, srv.admin)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("identity-auth server exited: %v", err)
	}
}

// only restricts a handler to one method, else 405 via the error envelope.
func (s *server) only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		h(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "identity-auth",
		"primary_kid": s.keys.primaryKID(), "keys": len(s.keys.kids()),
		"denylist_version": s.deny.snapshot().Version,
		"otel_exporter":    otel.ExporterMode(),
	})
}

func (s *server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	// Long-lived-cacheable but short enough that a rotation propagates fast.
	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, s.keys.jwks())
}

type registerReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in registerReq
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Email == "" || !strings.Contains(in.Email, "@") {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "valid email required", shoperr.Detail{Field: "email", Reason: "invalid"}))
		return
	}
	if len(in.Password) < 8 {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "password must be at least 8 characters", shoperr.Detail{Field: "password", Reason: "too_short"}))
		return
	}
	role := in.Role
	if role == "" {
		role = "customer"
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := user{ID: newID("usr"), Email: in.Email, PWHash: hash, Role: role}
	switch err := s.store.createUser(r.Context(), u); err {
	case nil:
	case errEmailTaken:
		s.fail(w, r, shoperr.New(codeEmailTaken, ""))
		return
	default:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user_id": u.ID, "email": strings.ToLower(in.Email), "role": role,
	})
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.store.userByEmail(r.Context(), in.Email)
	// Same error whether the user is absent or the password is wrong — no
	// account enumeration.
	if err != nil || !verifyPassword(in.Password, u.PWHash) {
		s.fail(w, r, shoperr.New(codeInvalidCreds, ""))
		return
	}
	s.issueSession(w, r, u)
}

// issueSession mints a fresh access+refresh pair and persists the session.
func (s *server) issueSession(w http.ResponseWriter, r *http.Request, u user) {
	sid := newID("ses")
	jti := newJTI()
	now := time.Now()
	access, kid, err := s.keys.sign(edgeauth.Claims{
		Iss: s.iss, Sub: u.ID, Role: u.Role, JTI: jti, Sid: sid,
		Iat: now.Unix(), Nbf: now.Unix(), Exp: now.Add(accessTTL).Unix(),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	refresh := newOpaqueToken()
	se := session{
		ID: sid, UserID: u.ID, Role: u.Role,
		RefreshHash: hashToken(refresh), AccessJTI: jti,
		ExpiresAt: now.Add(refreshTTL),
	}
	if err := s.store.createSession(r.Context(), se); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse(access, refresh, u, sid, kid, now))
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var in refreshReq
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.RefreshToken == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "refresh_token required", shoperr.Detail{Field: "refresh_token", Reason: "required"}))
		return
	}
	now := time.Now()
	newJTIv := newJTI()
	newRefresh := newOpaqueToken()
	prevJTI, se, err := s.store.rotateSession(r.Context(),
		hashToken(in.RefreshToken), hashToken(newRefresh), newJTIv, now.Add(refreshTTL))
	if err == errSessionState {
		s.fail(w, r, shoperr.New(codeSessionInval, ""))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Rotation invalidates the PRIOR access token immediately (defence in depth):
	// its jti goes on the denylist so a token minted before the rotation cannot
	// outlive the refresh it was paired with.
	s.deny.revoke(prevJTI)

	access, kid, err := s.keys.sign(edgeauth.Claims{
		Iss: s.iss, Sub: se.UserID, Role: se.Role, JTI: newJTIv, Sid: se.ID,
		Iat: now.Unix(), Nbf: now.Unix(), Exp: now.Add(accessTTL).Unix(),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse(access, newRefresh,
		user{ID: se.UserID, Role: se.Role}, se.ID, kid, now))
}

type revokeReq struct {
	RefreshToken string `json:"refresh_token"`
	JTI          string `json:"jti"`
}

func (s *server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var in revokeReq
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	var jti string
	switch {
	case in.JTI != "":
		// Revoke a specific access token directly (e.g. logout-all tooling).
		jti = in.JTI
	case in.RefreshToken != "":
		j, err := s.store.revokeByRefresh(r.Context(), hashToken(in.RefreshToken))
		if err == errSessionState {
			s.fail(w, r, shoperr.New(codeSessionInval, ""))
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		jti = j
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "refresh_token or jti required"))
		return
	}
	version := s.deny.revoke(jti)
	writeJSON(w, http.StatusOK, map[string]any{
		"revoked": true, "jti": jti, "denylist_version": version,
	})
}

func (s *server) handleDenylist(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deny.snapshot())
}

func (s *server) handleKeyRotate(w http.ResponseWriter, r *http.Request) {
	if !s.admin {
		s.fail(w, r, shoperr.New(shoperr.CodeForbidden, "key rotation disabled in this environment"))
		return
	}
	kid, err := s.keys.rotate()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"primary_kid": kid, "kids": s.keys.kids()})
}

func (s *server) handleKeyRetire(w http.ResponseWriter, r *http.Request) {
	if !s.admin {
		s.fail(w, r, shoperr.New(shoperr.CodeForbidden, "key rotation disabled in this environment"))
		return
	}
	retired, err := s.keys.retire()
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"retired_kid": retired, "primary_kid": s.keys.primaryKID(), "kids": s.keys.kids(),
	})
}

// tokenResponse is the shared login/refresh success body (02 §1: _at times,
// snake_case). expires_in is seconds; the client refreshes before expires_at.
func tokenResponse(access, refresh string, u user, sid, kid string, now time.Time) map[string]any {
	return map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL / time.Second),
		"expires_at":    now.Add(accessTTL).UTC().Format(time.RFC3339),
		"user_id":       u.ID,
		"session_id":    sid,
		"role":          u.Role,
		"kid":           kid,
	}
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	shoperr.WriteRequest(w, r, err, logging.TraceIDFromRequest)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return shoperr.New(shoperr.CodeValidation, "request body must be valid JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func selfCheck(u string) {
	resp, err := http.Get(u)
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	_ = resp.Body.Close()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
