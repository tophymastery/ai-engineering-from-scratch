// Command merchant-catalog is the V-T3 merchant catalog & menus service
// (01 §1: "Merchants, stores, menus, items, availability, opening hours"; owns
// the merchants/menus/items DB; publishes menu.updated + store.status_changed).
//
// The headline correctness property is OPTIMISTIC CONCURRENCY (02 §1): the menu
// and the store status are mutable resources that return a strong `ETag`; a
// PATCH/PUT whose `If-Match` no longer equals the current ETag is a stale write
// and is rejected with 412 STALE_WRITE. Every accepted mutation also writes its
// domain event to the transactional outbox in the SAME transaction (exactly-once
// publish; consumers = search + cart, keyed by merchant_id).
//
// Endpoints (02 §1 conventions, 02 §2 error envelope, libs/logging+otel+errors;
// the mutating endpoints are gated by the `catalog_v1` flag, libs/flags):
//
//	POST  /v1/merchants                                create a merchant (+ empty menu, CLOSED store)
//	GET   /v1/merchants/{merchant_id}/menu            read menu (returns ETag)
//	PATCH /v1/merchants/{merchant_id}/menu            edit menu (If-Match required → 412 on stale)
//	GET   /v1/merchants/{merchant_id}/store-status    read store status (returns ETag)
//	PUT   /v1/merchants/{merchant_id}/store-status    set store status (If-Match required → 412 on stale)
package main

import (
	"context"
	"encoding/json"
	"errors"
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

// errValidation is a store-side sentinel wrapped around domain validation
// failures; mapped to the shared VALIDATION (400) envelope by the handlers.
var errValidation = errors.New("validation")

// Domain error codes owned by this slice.
var (
	codeMerchantNotFound = shoperr.Register("MERCHANT_NOT_FOUND", 404, false, "No merchant exists for this id.")
	codeMerchantExists   = shoperr.Register("MERCHANT_ALREADY_EXISTS", 409, false, "A merchant already exists for this id.")
	codeIfMatchRequired  = shoperr.Register("IF_MATCH_REQUIRED", 428, false, "This mutating endpoint requires an If-Match header carrying the resource's current ETag.")
	codeCatalogDisabled  = shoperr.Register("CATALOG_DISABLED", 404, false, "The catalog_v1 feature is not enabled.")
)

type server struct {
	st      *store
	ev      *eventBuilder
	log     *logging.Logger
	flags   *flags.Set
	enabled bool // catalog_v1 default (per-request override still honoured in non-prod)
}

func main() {
	port := envOr("PORT", "8102")
	name := envOr("SERVICE_NAME", "merchant-catalog")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	ctx := context.Background()
	st, err := openStore(ctx, region)
	if err != nil {
		log.Fatalf("merchant-catalog: open store: %v", err)
	}

	fs := flags.FromEnv()
	srv := &server{
		st: st, ev: newEventBuilder(region),
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("catalog_v1", false),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/v1/merchants", srv.only(http.MethodPost, srv.handleCreateMerchant))
	mux.HandleFunc("/v1/merchants/", srv.handleMerchantSubtree)

	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("merchant-catalog %q on %s (env=%s region=%s catalog_v1=%v)", name, addr, env, region, srv.enabled)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("merchant-catalog server exited: %v", err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "merchant-catalog",
		"catalog_v1":    s.catalogEnabled(r),
		"otel_exporter": otel.ExporterMode(),
	})
}

// catalogEnabled resolves the catalog_v1 flag for this request: the default from
// env, with a per-request X-Flag-Override honoured in non-prod (libs/flags +
// libs/testhooks). Gating the mutating surface on it satisfies "ships dark;
// e2e runs with it on" (03 §5).
func (s *server) catalogEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "catalog_v1", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.catalogEnabled(r) {
		s.fail(w, r, shoperr.New(codeCatalogDisabled, ""))
		return false
	}
	return true
}

func (s *server) handleCreateMerchant(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w, r) {
		return
	}
	var in merchantInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	mv, err := s.st.createMerchant(r.Context(), s.ev, in, logging.TraceIDFromRequest(r))
	switch {
	case err == errDupMerchant:
		s.fail(w, r, shoperr.New(codeMerchantExists, ""))
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, mv)
	}
}

// handleMerchantSubtree routes /v1/merchants/{id}/menu and
// /v1/merchants/{id}/store-status.
func (s *server) handleMerchantSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/merchants/")
	switch {
	case strings.HasSuffix(suffix, "/menu"):
		mid := strings.TrimSuffix(suffix, "/menu")
		s.handleMenu(w, r, mid)
	case strings.HasSuffix(suffix, "/store-status"):
		mid := strings.TrimSuffix(suffix, "/store-status")
		s.handleStoreStatus(w, r, mid)
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown catalog path"))
	}
}

func (s *server) handleMenu(w http.ResponseWriter, r *http.Request, mid string) {
	switch r.Method {
	case http.MethodGet:
		mv, err := s.st.getMenu(r.Context(), mid)
		s.respondMenu(w, r, mv, err)
	case http.MethodPatch:
		if !s.requireEnabled(w, r) {
			return
		}
		ifMatch := r.Header.Get("If-Match")
		if ifMatch == "" {
			s.fail(w, r, shoperr.New(codeIfMatchRequired, ""))
			return
		}
		var patch menuPatch
		if err := decode(r, &patch); err != nil {
			s.fail(w, r, err)
			return
		}
		mv, err := s.st.patchMenu(r.Context(), s.ev, mid, ifMatch, patch, logging.TraceIDFromRequest(r))
		s.respondMenu(w, r, mv, err)
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
	}
}

func (s *server) respondMenu(w http.ResponseWriter, r *http.Request, mv menuView, err error) {
	if err != nil {
		s.failStore(w, r, err)
		return
	}
	w.Header().Set("ETag", mv.ETag)
	writeJSON(w, http.StatusOK, mv)
}

func (s *server) handleStoreStatus(w http.ResponseWriter, r *http.Request, mid string) {
	switch r.Method {
	case http.MethodGet:
		sv, err := s.st.getStoreStatus(r.Context(), mid)
		s.respondStatus(w, r, sv, err)
	case http.MethodPut:
		if !s.requireEnabled(w, r) {
			return
		}
		ifMatch := r.Header.Get("If-Match")
		if ifMatch == "" {
			s.fail(w, r, shoperr.New(codeIfMatchRequired, ""))
			return
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := decode(r, &body); err != nil {
			s.fail(w, r, err)
			return
		}
		sv, err := s.st.putStoreStatus(r.Context(), s.ev, mid, ifMatch, strings.ToUpper(body.Status), logging.TraceIDFromRequest(r))
		s.respondStatus(w, r, sv, err)
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
	}
}

func (s *server) respondStatus(w http.ResponseWriter, r *http.Request, sv storeStatusView, err error) {
	if err != nil {
		s.failStore(w, r, err)
		return
	}
	w.Header().Set("ETag", sv.ETag)
	writeJSON(w, http.StatusOK, sv)
}

// failStore maps store sentinels to the 02 §2 error envelope.
func (s *server) failStore(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == errNoMerchant:
		s.fail(w, r, shoperr.New(codeMerchantNotFound, ""))
	case err == errStaleWrite:
		s.fail(w, r, shoperr.New(shoperr.CodeStaleWrite, ""))
	case err == errBadStatus:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "status must be one of OPEN, BUSY, CLOSED",
			shoperr.Detail{Field: "status", Reason: "invalid_enum"}))
	case errors.Is(err, errValidation):
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, err.Error()))
	default:
		s.fail(w, r, err)
	}
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
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if err.Error() == "EOF" {
			return nil // empty body is allowed (e.g. PUT with defaults)
		}
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
