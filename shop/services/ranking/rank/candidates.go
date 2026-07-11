package rank

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// CandidateSource retrieves the top-N retrieval set for a browse point. In D17's
// two-phase design retrieval is the search service's job (OpenSearch top-500);
// ranking re-ranks that set. The source is injected so the ranker package stays
// testable without a live search service (tests pass a static source; the binary
// passes an HTTPCandidateSource pointed at the search slot).
type CandidateSource interface {
	Candidates(ctx context.Context, lat, lng float64, limit int) ([]Candidate, error)
}

// HTTPCandidateSource retrieves candidates from the search service's browse feed
// (GET /v1/customer/home?lat=&lng=&limit=), which returns the ALREADY-enriched
// feed items (store, rating, distance, delivery_fee, eta) — ranking re-orders
// them, so the customer feed is field-for-field what search produced, just ranked.
// This is exactly "consumes search contract stubs": ranking is a client of the
// published search.v1 browse contract.
type HTTPCandidateSource struct {
	BaseURL string // e.g. http://localhost:8103 (the search slot)
	Client  *http.Client
}

// NewHTTPCandidateSource builds a source with a bounded timeout so a slow search
// never stalls the browse feed (a slow retrieval degrades to whatever it returns).
func NewHTTPCandidateSource(baseURL string) *HTTPCandidateSource {
	return &HTTPCandidateSource{BaseURL: baseURL, Client: &http.Client{Timeout: 3 * time.Second}}
}

// homeFeedWire is the subset of the search browse response ranking consumes.
type homeFeedWire struct {
	Feed []Candidate `json:"feed"`
}

func (h *HTTPCandidateSource) Candidates(ctx context.Context, lat, lng float64, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 500
	}
	q := url.Values{}
	q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
	q.Set("lng", strconv.FormatFloat(lng, 'f', -1, 64))
	q.Set("limit", strconv.Itoa(limit))
	u := h.BaseURL + "/v1/customer/home?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search retrieval status %d", resp.StatusCode)
	}
	var body homeFeedWire
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Feed, nil
}
