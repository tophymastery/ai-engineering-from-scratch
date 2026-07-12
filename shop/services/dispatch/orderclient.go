package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// orderclient.go — the seam by which an ACCEPTED offer drives the order saga
// forward. When a driver accepts, dispatch emits dispatch.assigned; in a live
// topology the order saga consumes it off Kafka, but in the process-mode sandbox
// (separate in-memory brokers per process) dispatch PUSHES the envelope to the
// order slot's /v1/order-events inbox path — the same cross-process delivery
// merchant-queue uses to drive the saga (VERIFICATION §V-T12, disclosed). A noop
// client is used when ORDER_URL is unset (unit tests, all-stubs e2e).

// OrderClient delivers a produced dispatch.* envelope to the order saga.
type OrderClient interface {
	Deliver(ctx context.Context, env eventbus.Envelope) error
}

// noopOrderClient drops deliveries (no order slot wired).
type noopOrderClient struct{}

func (noopOrderClient) Deliver(context.Context, eventbus.Envelope) error { return nil }

// httpOrderClient POSTs the envelope to the order slot's /v1/order-events endpoint.
type httpOrderClient struct {
	base string
	hc   *http.Client
}

func newHTTPOrderClient(base string) *httpOrderClient {
	return &httpOrderClient{base: base, hc: &http.Client{Timeout: 5 * time.Second}}
}

func (c *httpOrderClient) Deliver(ctx context.Context, env eventbus.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/order-events", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
