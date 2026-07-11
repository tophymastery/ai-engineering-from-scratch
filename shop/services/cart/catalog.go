package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// catalog.go — the CATALOG CONTRACT surface cart validates against (01 §1 "cart:
// item validation against catalog"; the cart→merchant-catalog pact in
// contracts/pacts/cart__merchant-catalog.json). Two feeds keep cart's view of a
// merchant's menu current:
//
//   1. On-demand READ at add time (httpCatalog.fetchMenu) — GET
//      /v1/merchants/{id}/menu on merchant-catalog (the pinned pact interaction),
//      so a line item added to the cart is priced + gated from the AUTHORITATIVE
//      catalog even for a merchant the cart has never seen.
//   2. menu.updated EVENTS (events.go consumer) — a merchant edit repriced/flagged
//      every affected cart line within the freshness window (the "menu-change
//      revalidation reflected < 5 s" criterion).
//
// Both feeds land in catalogView, a last-write-wins (by menu `version`) cache of
// item price + availability. It is the single source cart prices a line from, so
// an add and a later revalidation agree.

// itemInfo is cart's cached knowledge of one catalog item: its price snapshot
// (integer minor units + ISO currency, 02 §1 Money) and availability, tagged
// with the menu `version` it came from (for LWW).
type itemInfo struct {
	Name      string
	Amount    int64
	Currency  string
	Available bool
	Version   int64
}

// catalogView is a concurrent, last-write-wins cache of merchant menus. Safe for
// concurrent use (add-path request goroutines + the menu.updated consumer
// goroutine read/write it under -race).
type catalogView struct {
	mu       sync.RWMutex
	menus    map[string]map[string]itemInfo // merchant_id -> item_id -> itemInfo
	versions map[string]int64               // merchant_id -> latest applied menu version (LWW guard)
}

func newCatalogView() *catalogView {
	return &catalogView{menus: map[string]map[string]itemInfo{}, versions: map[string]int64{}}
}

// applyMenu installs a merchant's full item set at a given version, last-write-
// wins: an older-or-equal version is ignored (events across the bus may arrive
// out of order; monotonic `version` orders them). Returns true when applied.
// items missing from a newer snapshot are dropped (the item was removed from the
// menu) — a cart line referencing a dropped item then revalidates to unavailable.
func (cv *catalogView) applyMenu(merchantID string, version int64, items map[string]itemInfo) bool {
	cv.mu.Lock()
	defer cv.mu.Unlock()
	if cur, ok := cv.versions[merchantID]; ok && version <= cur {
		return false // stale/duplicate menu snapshot — LWW
	}
	cp := make(map[string]itemInfo, len(items))
	for id, it := range items {
		it.Version = version
		cp[id] = it
	}
	cv.menus[merchantID] = cp
	cv.versions[merchantID] = version
	return true
}

// lookup returns the cached item info for a (merchant, item), and whether the
// merchant's menu is known at all (present=false ⇒ cart must fetch it).
func (cv *catalogView) lookup(merchantID, itemID string) (info itemInfo, itemKnown, merchantKnown bool) {
	cv.mu.RLock()
	defer cv.mu.RUnlock()
	m, ok := cv.menus[merchantID]
	if !ok {
		return itemInfo{}, false, false
	}
	it, ok := m[itemID]
	return it, ok, true
}

// version returns the latest applied menu version for a merchant (0 if unknown).
func (cv *catalogView) version(merchantID string) int64 {
	cv.mu.RLock()
	defer cv.mu.RUnlock()
	return cv.versions[merchantID]
}

// catalogFetcher fetches a merchant's authoritative menu on demand (the pact
// read). Production is httpCatalog against merchant-catalog; tests inject a fake.
type catalogFetcher interface {
	fetchMenu(ctx context.Context, merchantID string) (version int64, items map[string]itemInfo, err error)
}

// httpCatalog is the production fetcher: GET {base}/v1/merchants/{id}/menu on the
// merchant-catalog slot — exactly the interaction pinned by the cart pact. The
// menu read returns {version, items:[{item_id, name, price:{amount,currency},
// available}]} (the merchant-catalog menuView shape).
type httpCatalog struct {
	base   string
	client *http.Client
}

func newHTTPCatalog(base string) *httpCatalog {
	return &httpCatalog{base: base, client: &http.Client{Timeout: 3 * time.Second}}
}

// menuReadResponse mirrors merchant-catalog's menuView (the GET /menu body). The
// price is NESTED here (the HTTP read contract); the menu.updated EVENT payload
// carries the same numbers flattened (events.go) — both feed catalogView.
type menuReadResponse struct {
	MerchantID string `json:"merchant_id"`
	Version    int64  `json:"version"`
	Items      []struct {
		ItemID string `json:"item_id"`
		Name   string `json:"name"`
		Price  struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"price"`
		Available bool `json:"available"`
	} `json:"items"`
}

func (c *httpCatalog) fetchMenu(ctx context.Context, merchantID string) (int64, map[string]itemInfo, error) {
	u := c.base + "/v1/merchants/" + url.PathEscape(merchantID) + "/menu"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil, errMerchantUnknown
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("catalog fetch %s: status %d", merchantID, resp.StatusCode)
	}
	var mr menuReadResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return 0, nil, fmt.Errorf("catalog fetch %s: bad body: %w", merchantID, err)
	}
	items := make(map[string]itemInfo, len(mr.Items))
	for _, it := range mr.Items {
		items[it.ItemID] = itemInfo{
			Name: it.Name, Amount: it.Price.Amount, Currency: it.Price.Currency,
			Available: it.Available, Version: mr.Version,
		}
	}
	return mr.Version, items, nil
}
