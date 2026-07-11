// Command cart is the V-T7 Cart slice (01 §1: "Per-user carts; item validation
// against catalog"; the Marketplace team's cart service). It owns per-user carts
// backed by a Redis snapshot over a durable PostgreSQL store, validates + prices
// line items against the merchant-catalog contract, and revalidates them when a
// merchant changes a menu — consuming `menu.updated` events (02 §4.3).
//
// The two headline correctness properties (proved for real, under -race):
//
//   - OPTIMISTIC CONCURRENCY (02 §1): the cart is a mutable resource that returns
//     a strong `ETag`; an add/remove whose `If-Match` no longer equals the current
//     ETag is a stale write and is rejected with 412 STALE_WRITE (same
//     compare-and-swap-in-tx pattern V-T3 proved for menus). Under concurrent
//     edits exactly one writer wins; every stale writer gets 412.
//   - MENU-CHANGE REVALIDATION reflected < 5 s (01 §1 test criteria): cart consumes
//     `menu.updated` and reprices / flags every affected cart line within the
//     freshness window, so a merchant's price change or an item going unavailable
//     is reflected in the cart's subtotal.
//
// Endpoints (02 §1 conventions, 02 §2 error envelope, libs/logging+otel+errors;
// the mutating endpoints are gated by the `cart_v1` flag, libs/flags):
//
//	GET    /v1/carts/{cart_id}                       read the cart (returns ETag)
//	POST   /v1/carts/{cart_id}/items                 add an item (If-Match once the cart exists → 412 on stale)
//	DELETE /v1/carts/{cart_id}/items/{item_id}       remove an item (If-Match required → 412 on stale)
//	POST   /v1/menu-events                           inject a menu.updated event (the stub-event delivery path in the E2E env; the in-process bus feeds this in prod)
//
// Sandbox adaptations (disclosed in VERIFICATION.md §V-T7): the "Redis snapshot"
// tier is an in-process TTL store standing in for Redis (no daemon here); the PG
// store is in-memory SQLite in tests (production schema migrations/0001_cart.pg.sql);
// the menu.updated bus is the in-memory eventbus + inbox (no live Kafka), with an
// HTTP inject endpoint standing in for cross-process delivery in the E2E env. The
// ETag concurrency, snapshot/rehydrate, and menu-change revalidation LOGIC — the
// correctness of this slice — is real and fully tested.
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
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/flags"
	"github.com/shop-platform/shop/libs/logging"
	"github.com/shop-platform/shop/libs/otel"
	"github.com/shop-platform/shop/libs/testhooks"
)

// Domain error codes owned by this slice.
var (
	codeCartNotFound    = shoperr.Register("CART_NOT_FOUND", 404, false, "No cart exists for this id.")
	codeCartDisabled    = shoperr.Register("CART_DISABLED", 404, false, "The cart_v1 feature is not enabled.")
	codeIfMatchRequired = shoperr.Register("IF_MATCH_REQUIRED", 428, false, "This mutating endpoint requires an If-Match header carrying the cart's current ETag.")
	codeItemUnavailable = shoperr.Register("ITEM_UNAVAILABLE", 422, false, "This item is not currently available.")
	codeItemNotInMenu   = shoperr.Register("ITEM_NOT_IN_MENU", 422, false, "This item is not on the merchant's menu.")
	codeMerchantUnknown = shoperr.Register("MERCHANT_UNKNOWN", 404, false, "No catalog menu could be resolved for this merchant.")
	codeCurrencyMismatch = shoperr.Register("CART_CURRENCY_MISMATCH", 422, false, "The item's currency differs from the cart's currency.")
)

type server struct {
	st       *store
	view     *catalogView
	snap     *snapshotStore
	consumer *menuConsumer
	fetcher  catalogFetcher
	log      *logging.Logger
	flags    *flags.Set
	enabled  bool // cart_v1 default (per-request override still honoured in non-prod)
}

func main() {
	port := envOr("PORT", "8104")
	name := envOr("SERVICE_NAME", "cart")

	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}

	env := envOr("ENV", "dev")
	region := envOr("REGION", "local")
	catalogOrigin := envOr("CATALOG_URL", "http://localhost:8102") // merchant-catalog slot
	snapTTL := envDuration("CART_SNAPSHOT_TTL", 5*time.Second)     // "reflected < 5s" freshness window

	ctx := context.Background()
	clk := SystemClock{}
	view := newCatalogView()
	snap := newSnapshotStore(clk, snapTTL)
	st, err := openStore(ctx, region, clk, view, snap)
	if err != nil {
		log.Fatalf("cart: open store: %v", err)
	}
	consumer := newMenuConsumer(view, "cart", func(ctx context.Context, merchantID string, version int64) error {
		_, err := st.revalidateMerchant(ctx, merchantID, version)
		return err
	})

	fs := flags.FromEnv()
	srv := &server{
		st: st, view: view, snap: snap, consumer: consumer,
		fetcher: newHTTPCatalog(catalogOrigin),
		log: logging.New(logging.Config{
			Service: name, Version: envOr("SERVICE_VERSION", "0.0.0-dev"),
			Env: env, Region: region, SampleRate: 1.0,
		}),
		flags:   fs,
		enabled: fs.Bool("cart_v1", false),
	}

	mux := srv.mux()
	handler := otel.Middleware(srv.log.Middleware(nil)(testhooks.Middleware(mux)))

	addr := ":" + port
	log.Printf("cart %q on %s (env=%s region=%s cart_v1=%v catalog=%s snap_ttl=%s)",
		name, addr, env, region, srv.enabled, catalogOrigin, snapTTL)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("cart server exited: %v", err)
	}
}

// mux builds the routing table (kept in sync with main.go's handler wiring; the
// test rebuilds the same set, as merchant-catalog/identity-profile do).
func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/carts/", s.handleCartsSubtree)
	mux.HandleFunc("/v1/menu-events", s.only(http.MethodPost, s.handleMenuEvent))
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "cart",
		"cart_v1":       s.cartEnabled(r),
		"otel_exporter": otel.ExporterMode(),
	})
}

// cartEnabled resolves the cart_v1 flag for this request: the env default, with a
// per-request X-Flag-Override honoured in non-prod (libs/flags + libs/testhooks).
// Gating the mutating surface on it satisfies "ships dark; e2e runs with it on".
func (s *server) cartEnabled(r *http.Request) bool {
	return s.flags.BoolCtx(r.Context(), "cart_v1", s.enabled)
}

func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.cartEnabled(r) {
		s.fail(w, r, shoperr.New(codeCartDisabled, ""))
		return false
	}
	return true
}

// handleCartsSubtree routes:
//
//	GET    /v1/carts/{id}
//	POST   /v1/carts/{id}/items
//	DELETE /v1/carts/{id}/items/{item_id}
func (s *server) handleCartsSubtree(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/carts/")
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "cart id path segment required"))
		return
	}
	parts := strings.Split(suffix, "/")
	switch {
	case len(parts) == 1: // /v1/carts/{id}
		s.handleGetCart(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "items": // /v1/carts/{id}/items
		s.handleAddItem(w, r, parts[0])
	case len(parts) == 3 && parts[1] == "items": // /v1/carts/{id}/items/{item_id}
		s.handleRemoveItem(w, r, parts[0], parts[2])
	default:
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "unknown cart path"))
	}
}

func (s *server) handleGetCart(w http.ResponseWriter, r *http.Request, cartID string) {
	if r.Method != http.MethodGet {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	v, err := s.st.getCartView(r.Context(), cartID)
	s.respond(w, r, v, err)
}

func (s *server) handleAddItem(w http.ResponseWriter, r *http.Request, cartID string) {
	if r.Method != http.MethodPost {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	var in addInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.ItemID == "" || in.MerchantID == "" {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "item_id and merchant_id are required",
			shoperr.Detail{Field: "merchant_id", Reason: "required"}))
		return
	}
	// Resolve the item's authoritative price + availability from the catalog
	// (catalogView, filled on demand by the cart→merchant-catalog pact read). This
	// is the "item validation against catalog" step (01 §1).
	priced, err := s.resolveItem(r.Context(), in.MerchantID, in.ItemID)
	if err != nil {
		s.failStore(w, r, err)
		return
	}
	ifMatch := r.Header.Get("If-Match")
	v, err := s.st.addItem(r.Context(), cartID, ifMatch, in, priced)
	s.respond(w, r, v, err)
}

func (s *server) handleRemoveItem(w http.ResponseWriter, r *http.Request, cartID, itemID string) {
	if r.Method != http.MethodDelete {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "method not allowed"))
		return
	}
	if !s.requireEnabled(w, r) {
		return
	}
	ifMatch := r.Header.Get("If-Match")
	v, err := s.st.removeItem(r.Context(), cartID, ifMatch, itemID)
	s.respond(w, r, v, err)
}

// resolveItem returns the authoritative catalog info for a (merchant, item),
// fetching the merchant's menu on demand (the pact read) when cart's view does
// not yet know the item.
func (s *server) resolveItem(ctx context.Context, merchantID, itemID string) (itemInfo, error) {
	info, itemKnown, merchantKnown := s.view.lookup(merchantID, itemID)
	if merchantKnown && itemKnown {
		return info, nil
	}
	version, items, err := s.fetcher.fetchMenu(ctx, merchantID)
	if err != nil {
		return itemInfo{}, err // errMerchantUnknown or transport error
	}
	s.view.applyMenu(merchantID, version, items)
	info, itemKnown, _ = s.view.lookup(merchantID, itemID)
	if !itemKnown {
		return itemInfo{}, errItemNotInMenu
	}
	return info, nil
}

// handleMenuEvent injects a menu.updated envelope through the consumer (exactly-
// once effect via the inbox → catalogView LWW → revalidate affected carts). In
// the E2E env this is the stub-event delivery seam (no cross-process Kafka); in
// production the in-memory eventbus subscription feeds the same consumer. Returns
// 202 (accepted, applied asynchronously to the read model).
func (s *server) handleMenuEvent(w http.ResponseWriter, r *http.Request) {
	var env eventbus.Envelope
	if err := decode(r, &env); err != nil {
		s.fail(w, r, err)
		return
	}
	if env.EventType == "" {
		env.EventType = TopicMenuUpdated
	}
	msg, err := eventbus.NewMessage(env.EventType, env)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "bad event envelope"))
		return
	}
	if err := s.consumer.Handle(r.Context(), msg); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"applied": true, "event_id": env.EventID})
}

func (s *server) respond(w http.ResponseWriter, r *http.Request, v cartView, err error) {
	if err != nil {
		s.failStore(w, r, err)
		return
	}
	w.Header().Set("ETag", v.ETag)
	writeJSON(w, http.StatusOK, v)
}

// failStore maps store sentinels to the 02 §2 error envelope.
func (s *server) failStore(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == errCartNotFound:
		s.fail(w, r, shoperr.New(codeCartNotFound, ""))
	case err == errStaleWrite:
		s.fail(w, r, shoperr.New(shoperr.CodeStaleWrite, ""))
	case err == errIfMatchRequired:
		s.fail(w, r, shoperr.New(codeIfMatchRequired, ""))
	case err == errItemUnavailable:
		s.fail(w, r, shoperr.New(codeItemUnavailable, ""))
	case err == errItemNotInMenu:
		s.fail(w, r, shoperr.New(codeItemNotInMenu, ""))
	case err == errMerchantUnknown:
		s.fail(w, r, shoperr.New(codeMerchantUnknown, ""))
	case err == errMixedCurrency:
		s.fail(w, r, shoperr.New(codeCurrencyMismatch, ""))
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
			return nil // empty body allowed
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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
