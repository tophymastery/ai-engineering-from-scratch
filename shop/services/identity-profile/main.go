// Command identity-profile is the V-T2 profile, residency & erasure service
// (implements D3). It OWNS customer PII (name, phone, email, address) and is the
// ONLY place plaintext PII ever exists — and then only in memory, at the request
// boundary. At rest, every PII field is AES-256-GCM ciphertext under a per-user
// data-encryption key (DEK); the wrapped DEK is the crypto-shred target.
//
// Three D3 guarantees are load-bearing and enforced in code + CI:
//
//  1. Residency — PII lives only in the owning jurisdiction's cell store
//     (in-country for ID/VN). A process serves only its homed cells; a request
//     tagged for a non-owning cell is refused (403), mirroring the k8s
//     NetworkPolicy (deploy/base/identity-profile/networkpolicy.yaml).
//  2. Token-only events — every emitted event / order snapshot carries usr_/adr_
//     tokens, never PII (proven by tools/piiscan over golden traffic).
//  3. Crypto-shredding erasure — POST /v1/profiles/{usr}:erase destroys the DEK
//     across the primary store + backups, making all PII ciphertext (including
//     immutable backups and pre-token event copies) permanently unreadable,
//     while usr_/adr_ tokens stay valid so token-keyed order history still
//     replays.
//
// Endpoints (02 §1 conventions, 02 §2 error envelope, libs/logging+otel+errors):
//
//	POST /v1/profiles                      create a profile (+ addresses)
//	GET  /v1/profiles/{usr}                read (decrypted) profile
//	PUT  /v1/profiles/{usr}                update mutable PII
//	POST /v1/profiles/{usr}/addresses      add an address (mints adr_ token)
//	POST /v1/profiles/{usr}:erase          crypto-shred erasure (right to erasure)
//	GET  /v1/tokens/{tok}                  resolve a usr_/adr_ token — NON-PII ref
//	POST /v1/orders:replay                 reconstruct an order from a token-only snapshot
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

// Domain error codes owned by this slice.
var (
	codeProfileNotFound = shoperr.Register("PROFILE_NOT_FOUND", 404, false, "No profile exists for this user token.")
	codeProfileExists   = shoperr.Register("PROFILE_ALREADY_EXISTS", 409, false, "A profile already exists for this user token.")
	codeProfileErased   = shoperr.Register("PROFILE_ERASED", 410, false, "This profile has been erased; its PII is unrecoverable (crypto-shredded).")
	codeResidency       = shoperr.Register("PROFILE_RESIDENCY_VIOLATION", 403, false, "This cell does not serve that jurisdiction; PII is only accessible from its owning cell.")
)

type server struct {
	stores     *stores
	kr         *keyring
	ev         *eventBuilder
	log        *logging.Logger
	defaultJur string
	homed      map[string]bool
}

func main() {
	port := envOr("PORT", "8113")
	name := envOr("SERVICE_NAME", "identity-profile")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	emitGolden := flag.String("emit-golden", "", "generate golden PII traffic (events+logs) into DIR for the PII scanner, then exit")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	// Jurisdictions this process is homed for (one cell may serve one). Default
	// covers the residency jurisdictions plus a global fallback.
	jurs := splitCSV(envOr("PROFILE_JURISDICTIONS", "ID,VN,SG,TH,MY,PH"))
	kr, err := loadKeyring()
	if err != nil {
		log.Fatalf("identity-profile: keyring: %v", err)
	}
	ctx := context.Background()
	st, err := openStores(ctx, kr, jurs)
	if err != nil {
		log.Fatalf("identity-profile: open stores: %v", err)
	}

	if *emitGolden != "" {
		if err := runEmitGolden(ctx, st, kr, region, *emitGolden); err != nil {
			log.Fatalf("identity-profile: emit-golden: %v", err)
		}
		return
	}

	srv := &server{
		stores: st, kr: kr, ev: newEventBuilder(region),
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		defaultJur: jurs[0],
		homed:      homedSet(jurs),
	}
	_ = flags.FromEnv()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/profiles", srv.only(http.MethodPost, srv.handleCreate))
	mux.HandleFunc("/v1/profiles/", srv.handleProfileSubtree)
	mux.HandleFunc("/v1/tokens/", srv.only(http.MethodGet, srv.handleResolve))
	mux.HandleFunc("/v1/orders:replay", srv.only(http.MethodPost, srv.handleReplay))

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("identity-profile %q on %s (env=%s cells=%v)", name, addr, env, jurs)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("identity-profile server exited: %v", err)
	}
}

// cellFor resolves the owning cell store from the request's X-Cell header (falls
// back to the service default). A jurisdiction this process is not homed for is a
// residency violation (403) — the app-layer twin of the NetworkPolicy.
func (s *server) cellFor(r *http.Request) (*cellStore, error) {
	jur := r.Header.Get("X-Cell")
	if jur == "" {
		jur = s.defaultJur
	}
	if !s.homed[jur] {
		return nil, shoperr.New(codeResidency, "cell "+jur+" is not served here")
	}
	cs := s.stores.cell(jur)
	if cs == nil {
		return nil, shoperr.New(codeResidency, "no store for cell "+jur)
	}
	return cs, nil
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "identity-profile",
		"cells":         keysOf(s.homed),
		"otel_exporter": otel.ExporterMode(),
	})
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in profileInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Jurisdiction == "" {
		in.Jurisdiction = r.Header.Get("X-Cell")
	}
	if in.Jurisdiction == "" {
		in.Jurisdiction = s.defaultJur
	}
	if !s.homed[in.Jurisdiction] {
		s.fail(w, r, shoperr.New(codeResidency, "cell "+in.Jurisdiction+" is not served here"))
		return
	}
	if in.UserToken == "" {
		in.UserToken = newToken("usr")
	} else if tokenKind(in.UserToken) != "user" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "user_token must be a usr_ token", shoperr.Detail{Field: "user_token", Reason: "bad_prefix"}))
		return
	}
	cs := s.stores.cell(in.Jurisdiction)
	pv, err := cs.createProfile(r.Context(), s.kr, in, s.ev)
	if err == errDupProfile {
		s.fail(w, r, shoperr.New(codeProfileExists, ""))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, pv)
}

// handleProfileSubtree routes everything under /v1/profiles/<...>.
func (s *server) handleProfileSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/profiles/")
	cs, err := s.cellFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch {
	case strings.HasSuffix(suffix, ":erase"):
		tok := strings.TrimSuffix(suffix, ":erase")
		if r.Method != http.MethodPost {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		s.handleErase(w, r, cs, tok)
	case strings.HasSuffix(suffix, "/addresses"):
		tok := strings.TrimSuffix(suffix, "/addresses")
		if r.Method != http.MethodPost {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		s.handleAddAddress(w, r, cs, tok)
	default:
		tok := suffix
		switch r.Method {
		case http.MethodGet:
			s.handleGet(w, r, cs, tok)
		case http.MethodPut:
			s.handleUpdate(w, r, cs, tok)
		default:
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		}
	}
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request, cs *cellStore, tok string) {
	pv, err := cs.getProfile(r.Context(), s.kr, tok)
	switch {
	case err == errNoProfile:
		s.fail(w, r, shoperr.New(codeProfileNotFound, ""))
	case err == errKeyDestroyed:
		s.fail(w, r, shoperr.New(codeProfileErased, ""))
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, pv)
	}
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request, cs *cellStore, tok string) {
	var in profileInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	pv, err := cs.updateProfile(r.Context(), s.kr, tok, in, s.ev)
	switch {
	case err == errNoProfile:
		s.fail(w, r, shoperr.New(codeProfileNotFound, ""))
	case err == errKeyDestroyed:
		s.fail(w, r, shoperr.New(codeProfileErased, ""))
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, pv)
	}
}

func (s *server) handleAddAddress(w http.ResponseWriter, r *http.Request, cs *cellStore, tok string) {
	var a addressInput
	if err := decode(r, &a); err != nil {
		s.fail(w, r, err)
		return
	}
	av, err := cs.addAddress(r.Context(), s.kr, tok, a, s.ev)
	switch {
	case err == errNoProfile:
		s.fail(w, r, shoperr.New(codeProfileNotFound, ""))
	case err == errKeyDestroyed:
		s.fail(w, r, shoperr.New(codeProfileErased, ""))
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, av)
	}
}

func (s *server) handleErase(w http.ResponseWriter, r *http.Request, cs *cellStore, tok string) {
	receipt, err := cs.erase(r.Context(), tok, s.ev)
	switch {
	case err == errNoProfile:
		s.fail(w, r, shoperr.New(codeProfileNotFound, ""))
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, receipt)
	}
}

func (s *server) handleResolve(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/v1/tokens/")
	if tokenKind(tok) == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "token must be a usr_ or adr_ token"))
		return
	}
	cs, err := s.cellFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ref, err := cs.resolveToken(r.Context(), tok)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *server) handleReplay(w http.ResponseWriter, r *http.Request) {
	var snap orderSnapshot
	if err := decode(r, &snap); err != nil {
		s.fail(w, r, err)
		return
	}
	if snap.Jurisdiction == "" {
		snap.Jurisdiction = s.defaultJur
	}
	out, err := replayOrder(r.Context(), s.stores, snap)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// only restricts a handler to one method, else 400 via the error envelope.
func (s *server) only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
			return
		}
		h(w, r)
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

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func homedSet(jurs []string) map[string]bool {
	m := map[string]bool{}
	for _, j := range jurs {
		m[j] = true
	}
	return m
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
