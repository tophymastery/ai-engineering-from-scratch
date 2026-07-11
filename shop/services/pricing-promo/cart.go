package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// cart.go — the CART CONTRACT surface pricing prices against (V-T8 scope:
// "consumes cart contract stubs"). To price a cart the engine needs its subtotal
// + currency, which pricing reads from the cart slot's GET /v1/carts/{cart_id}
// (the cart.v1 contract, V-T7). In the shared E2E env the cart slot is a stubgen
// stub until V-T7's binary merges, then the same port serves the real cart — the
// pricing-promo → cart pact (contracts/pacts/pricing-promo__cart.json) pins the
// read shape either way. Tests inject a fake cartFetcher (no network).

// cartSnapshot is the slice of the cart pricing needs. It maps the fields cart's
// GET /v1/carts/{id} returns (services/cart cartView): cart_id, currency, and
// the subtotal Money. Extra cart fields (items, etag, version) are ignored —
// forward-compatible per 02 §1.
type cartSnapshot struct {
	CartID   string
	Subtotal int64
	Currency string
}

// errCartNotFound / errCartUnavailable are the transport sentinels, mapped to
// 404 / 503 by the handler.
var (
	errCartNotFound    = errors.New("no such cart")
	errCartUnavailable = errors.New("cart service unavailable")
)

// cartFetcher fetches a cart snapshot by id. Production is httpCart against the
// cart slot; tests inject a fake.
type cartFetcher interface {
	fetchCart(ctx context.Context, cartID string) (cartSnapshot, error)
}

// httpCart is the production fetcher: GET {base}/v1/carts/{id} on the cart slot —
// exactly the interaction pinned by the pricing-promo → cart pact.
type httpCart struct {
	base   string
	client *http.Client
}

func newHTTPCart(base string) *httpCart {
	return &httpCart{base: base, client: &http.Client{Timeout: 3 * time.Second}}
}

// cartReadResponse mirrors cart's cartView (the GET /v1/carts/{id} body): the
// subtotal is a nested Money {amount, currency}.
type cartReadResponse struct {
	CartID   string `json:"cart_id"`
	Currency string `json:"currency"`
	Subtotal struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	} `json:"subtotal"`
}

func (c *httpCart) fetchCart(ctx context.Context, cartID string) (cartSnapshot, error) {
	u := c.base + "/v1/carts/" + url.PathEscape(cartID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return cartSnapshot{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return cartSnapshot{}, errCartUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return cartSnapshot{}, errCartNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return cartSnapshot{}, fmt.Errorf("%w: cart %s status %d", errCartUnavailable, cartID, resp.StatusCode)
	}
	var cr cartReadResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return cartSnapshot{}, fmt.Errorf("cart fetch %s: bad body: %w", cartID, err)
	}
	cur := cr.Currency
	if cur == "" {
		cur = cr.Subtotal.Currency
	}
	return cartSnapshot{CartID: cartID, Subtotal: cr.Subtotal.Amount, Currency: cur}, nil
}
