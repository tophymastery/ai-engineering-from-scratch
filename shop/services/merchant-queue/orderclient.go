package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// orderclient.go — the seam by which a merchant ACCEPT drives the order saga
// forward (so "accept → saga proceeds"). The merchant-queue owns the read model +
// the admission gate; the order service (V-T9) owns the authoritative state
// machine. On an admitted accept the queue calls the order service's
// POST /v1/orders/{id}:accept (the published order.v1 contract action verb),
// which applies PAID→ACCEPTED and emits order.accepted — the saga proceeds. In
// the shared E2E env this is the real order slot; in tests it is a fakeOrderClient.

// acceptOutcome is the classified result of driving the saga.
type acceptOutcome int

const (
	acceptOK        acceptOutcome = iota // saga advanced (PAID→ACCEPTED / rejected)
	acceptConflict                        // order not in the right state (409) — a client error, not 5xx
	acceptUpstream                        // order service unreachable / 5xx
)

// OrderClient drives the order saga on merchant accept/reject.
type OrderClient interface {
	Accept(ctx context.Context, orderID string) acceptOutcome
	Reject(ctx context.Context, orderID string) acceptOutcome
}

// httpOrderClient calls the real order service's :accept / :reject action verbs.
type httpOrderClient struct {
	base string
	hc   *http.Client
}

func newHTTPOrderClient(base string) *httpOrderClient {
	return &httpOrderClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 5 * time.Second}}
}

func (c *httpOrderClient) action(ctx context.Context, orderID, verb string) acceptOutcome {
	url := fmt.Sprintf("%s/v1/orders/%s:%s", c.base, orderID, verb)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"actor":"merchant-bff"}`))
	if err != nil {
		return acceptUpstream
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return acceptUpstream
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return acceptOK
	case resp.StatusCode == http.StatusConflict || (resp.StatusCode >= 400 && resp.StatusCode < 500):
		return acceptConflict
	default:
		return acceptUpstream
	}
}

func (c *httpOrderClient) Accept(ctx context.Context, orderID string) acceptOutcome {
	return c.action(ctx, orderID, "accept")
}

func (c *httpOrderClient) Reject(ctx context.Context, orderID string) acceptOutcome {
	return c.action(ctx, orderID, "reject")
}

// noopOrderClient is used when no ORDER_URL is configured (the accept still meters
// admission + projects locally, but there is no saga to drive). It reports OK so a
// standalone merchant-queue demo doesn't error.
type noopOrderClient struct{}

func (noopOrderClient) Accept(context.Context, string) acceptOutcome { return acceptOK }
func (noopOrderClient) Reject(context.Context, string) acceptOutcome { return acceptOK }
