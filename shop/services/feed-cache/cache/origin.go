package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// origin.go — the ORIGIN behind the caches: the expensive source a cache exists
// to protect. For the geo-tile feed cache the origin is the ranking browse feed
// (customer-bff → feed-cache → ranking → search); for the merchant page it is
// merchant-catalog. The interface is injected so the cache tiers are tested
// against an in-process counting origin (the real "exactly 1 origin fetch" proof
// counts calls here) while the binary uses HTTPOrigin.
type Origin interface {
	// Fetch retrieves the authoritative bytes for key. header carries request
	// headers that must reach the origin (e.g. a forwarded X-Flag-Override on a
	// cache-bypass request). key is the cache key; the concrete origin maps it to
	// a request (a tile → lat/lng query, a merchant id → a menu path).
	Fetch(ctx context.Context, key string, header http.Header) ([]byte, error)
}

// HTTPOrigin fetches from an upstream HTTP service. urlFor maps a cache key to the
// full request URL, so one HTTPOrigin type serves both the feed (key = tile,
// url = /v1/customer/home?lat=&lng=) and the merchant page (key = merchant_id,
// url = /v1/merchants/{id}/menu).
type HTTPOrigin struct {
	Client   *http.Client
	URLFor   func(key string) string
	Forward  []string // request headers copied through to the origin (e.g. X-Flag-Override)
	MaxBytes int64
}

// NewHTTPOrigin builds an origin with a bounded timeout (a slow origin must not
// stall a whole stampede that is waiting on the single leader fetch).
func NewHTTPOrigin(urlFor func(key string) string, forward ...string) *HTTPOrigin {
	return &HTTPOrigin{
		Client:   &http.Client{Timeout: 3 * time.Second},
		URLFor:   urlFor,
		Forward:  forward,
		MaxBytes: 8 << 20,
	}
}

// Fetch performs the upstream GET and returns the raw body.
func (o *HTTPOrigin) Fetch(ctx context.Context, key string, header http.Header) ([]byte, error) {
	u := o.URLFor(key)
	if _, err := url.Parse(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range o.Forward {
		if v := header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("origin status %d for %q", resp.StatusCode, key)
	}
	max := o.MaxBytes
	if max <= 0 {
		max = 8 << 20
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}
