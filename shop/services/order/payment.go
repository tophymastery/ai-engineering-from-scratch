package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// payment.go is the saga's payment adapter (01 §4 steps 2/6: authorize on
// checkout, capture on delivery, void/refund on compensation). The order service
// orchestrates against the PUBLISHED payment contract + the payment-sim fake
// (S-T7); the real payment service (V-T10) replaces the fake behind the same
// contract in the shared E2E env. Here the interface is what the saga depends on,
// so the exactly-once tests can inject a COUNTING fake and assert "exactly one
// charge" by counting Authorize calls.

// PaymentClient is the saga's dependency on the payment slot.
type PaymentClient interface {
	Authorize(ctx context.Context, orderID string, amount money) (authID string, err error)
	Capture(ctx context.Context, authID string, amount money) (captureID string, err error)
	Void(ctx context.Context, authID string) error
	Refund(ctx context.Context, captureID string, amount money) error
}

// countingPaymentClient is the deterministic in-test PSP: it records how many
// times each money operation was invoked so a test can assert exactly-once
// charge / exactly-once void. Safe under -race.
type countingPaymentClient struct {
	mu        sync.Mutex
	authorize int
	capture   int
	void      int
	refund    int
	failNext  bool // force the next Authorize to fail (decline branch)
}

func newCountingPaymentClient() *countingPaymentClient { return &countingPaymentClient{} }

func (c *countingPaymentClient) Authorize(_ context.Context, orderID string, _ money) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext {
		c.failNext = false
		return "", fmt.Errorf("payment declined")
	}
	c.authorize++
	return "pay_auth_" + orderID, nil
}

func (c *countingPaymentClient) Capture(_ context.Context, authID string, _ money) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capture++
	return "pay_cap_" + authID, nil
}

func (c *countingPaymentClient) Void(_ context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.void++
	return nil
}

func (c *countingPaymentClient) Refund(_ context.Context, _ string, _ money) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refund++
	return nil
}

// counts snapshots the counters (for assertions).
func (c *countingPaymentClient) counts() (auth, cap, void, refund int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authorize, c.capture, c.void, c.refund
}

// httpPaymentClient calls the payment-sim fake (or the real payment service)
// over the published PSP contract. Used in process-mode / E2E. Best-effort: a
// PSP error is surfaced to the saga, which decides the compensation.
type httpPaymentClient struct {
	base string
	c    *http.Client
}

func newHTTPPaymentClient(base string) *httpPaymentClient {
	return &httpPaymentClient{base: base, c: &http.Client{Timeout: 3 * time.Second}}
}

func (h *httpPaymentClient) post(ctx context.Context, path string, body any) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode >= 300 {
		return m, fmt.Errorf("payment %s: status %d", path, resp.StatusCode)
	}
	return m, nil
}

func (h *httpPaymentClient) Authorize(ctx context.Context, orderID string, amount money) (string, error) {
	m, err := h.post(ctx, "/v1/psp/authorize", map[string]any{
		"card_number": "4111111111111111", "amount": amount, "order_ref": orderID,
	})
	if err != nil {
		return "", err
	}
	if id, ok := m["auth_id"].(string); ok {
		return id, nil
	}
	return "", nil
}

func (h *httpPaymentClient) Capture(ctx context.Context, authID string, amount money) (string, error) {
	m, err := h.post(ctx, "/v1/psp/capture", map[string]any{"auth_id": authID, "amount": amount})
	if err != nil {
		return "", err
	}
	if id, ok := m["capture_id"].(string); ok {
		return id, nil
	}
	return "", nil
}

func (h *httpPaymentClient) Void(ctx context.Context, authID string) error {
	// payment-sim models a void as a refund of the held auth; the real payment
	// service exposes an explicit void. Best-effort on the fake.
	_, err := h.post(ctx, "/v1/psp/refund", map[string]any{"auth_id": authID})
	return err
}

func (h *httpPaymentClient) Refund(ctx context.Context, captureID string, amount money) error {
	_, err := h.post(ctx, "/v1/psp/refund", map[string]any{"capture_id": captureID, "amount": amount})
	return err
}
