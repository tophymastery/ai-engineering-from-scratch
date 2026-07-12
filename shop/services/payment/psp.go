package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// psp.go is the payment service's DOWNSTREAM PSP adapter (03 §5). The service
// exposes the shop-internal authorize/capture/refund contract (payment.v1) and
// delegates to a PSP behind this interface — the payment-sim fake (S-T7) in the
// sandbox, a real acquirer in production. Making the PSP an interface means the
// money-correctness tests inject a COUNTING adapter and assert "exactly one
// charge" by counting Authorize calls, deterministically and under -race.

// pspErrorKind classifies a PSP failure so the caller (and the circuit breaker)
// knows whether to compensate (decline: terminal) or retry (timeout/unavailable).
type pspErrorKind string

const (
	pspDeclined    pspErrorKind = "DECLINED"    // issuer said no — terminal, do NOT retry
	pspTimeout     pspErrorKind = "TIMEOUT"     // no answer in time — retryable
	pspUnavailable pspErrorKind = "UNAVAILABLE" // transport/5xx — retryable
)

// pspError is a typed PSP failure.
type pspError struct {
	Kind pspErrorKind
	Msg  string
}

func (e *pspError) Error() string { return string(e.Kind) + ": " + e.Msg }

// retryable reports whether an error is worth retrying (timeout/unavailable).
func retryable(err error) bool {
	pe, ok := err.(*pspError)
	return ok && (pe.Kind == pspTimeout || pe.Kind == pspUnavailable)
}

// declined reports whether an error is a hard issuer decline (compensate, don't retry).
func declined(err error) bool {
	pe, ok := err.(*pspError)
	return ok && pe.Kind == pspDeclined
}

// authResult is the PSP authorize outcome.
type authResult struct {
	AuthID string
	Last4  string
}

// PSP is the payment service's dependency on the acquirer slot.
type PSP interface {
	Authorize(ctx context.Context, orderRef, card string, amount money, callbackURL string) (authResult, error)
	Capture(ctx context.Context, authID string, amount money, callbackURL string) (captureID string, err error)
	Refund(ctx context.Context, captureID string, amount money, callbackURL string) (refundID string, err error)
}

// --- counting / scriptable in-test PSP --------------------------------------

// countingPSP is the deterministic in-test acquirer: it records how many times
// each money op was invoked (so a test asserts exactly-once charge) and can be
// scripted to decline (card …0002) or time out (card …0044). Safe under -race.
type countingPSP struct {
	mu        sync.Mutex
	authorize int
	capture   int
	refund    int
	seq       int64
	// timeoutBudget: authorize returns TIMEOUT this many more times before the
	// card would eventually succeed — models a flaky issuer for the retry test.
	timeoutBudget int
}

func newCountingPSP() *countingPSP { return &countingPSP{} }

// counts snapshots the counters (for assertions).
func (c *countingPSP) counts() (auth, capt, refund int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authorize, c.capture, c.refund
}

func (c *countingPSP) Authorize(_ context.Context, orderRef, card string, _ money, _ string) (authResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.HasSuffix(card, "0002") {
		return authResult{}, &pspError{pspDeclined, "card ending 0002 declined"}
	}
	if strings.HasSuffix(card, "0044") {
		return authResult{}, &pspError{pspTimeout, "card ending 0044 timed out"}
	}
	if c.timeoutBudget > 0 {
		c.timeoutBudget--
		return authResult{}, &pspError{pspTimeout, "issuer timeout (flaky)"}
	}
	c.authorize++
	c.seq++
	return authResult{AuthID: fmt.Sprintf("psp_auth_%s_%d", orderRef, c.seq), Last4: lastN(card, 4)}, nil
}

func (c *countingPSP) Capture(_ context.Context, authID string, _ money, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capture++
	c.seq++
	return fmt.Sprintf("psp_cap_%s_%d", authID, c.seq), nil
}

func (c *countingPSP) Refund(_ context.Context, captureID string, _ money, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refund++
	c.seq++
	return fmt.Sprintf("psp_ref_%s_%d", captureID, c.seq), nil
}

// --- HTTP adapter over the payment-sim PSP contract -------------------------

// httpPSP calls the payment-sim fake (or a real acquirer) over the published PSP
// contract (contracts/openapi/payment-sim.v1.yaml). 402 ⇒ DECLINED, 504 ⇒
// TIMEOUT, other 5xx/transport ⇒ UNAVAILABLE. callbackURL is passed so the PSP
// fires its async webhooks back at the payment service.
type httpPSP struct {
	base string
	c    *http.Client
}

func newHTTPPSP(base string) *httpPSP {
	return &httpPSP{base: base, c: &http.Client{Timeout: 3 * time.Second}}
}

func (h *httpPSP) post(ctx context.Context, path string, body any) (map[string]any, int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, &pspError{pspUnavailable, err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.c.Do(req)
	if err != nil {
		// A client-side deadline exceeded surfaces as a timeout to the caller.
		if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
			return nil, 0, &pspError{pspTimeout, err.Error()}
		}
		return nil, 0, &pspError{pspUnavailable, err.Error()}
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusPaymentRequired:
			return m, resp.StatusCode, &pspError{pspDeclined, "issuer declined"}
		case http.StatusGatewayTimeout:
			return m, resp.StatusCode, &pspError{pspTimeout, "issuer timeout"}
		default:
			return m, resp.StatusCode, &pspError{pspUnavailable, fmt.Sprintf("psp %s status %d", path, resp.StatusCode)}
		}
	}
	return m, resp.StatusCode, nil
}

func (h *httpPSP) Authorize(ctx context.Context, orderRef, card string, amount money, callbackURL string) (authResult, error) {
	m, _, err := h.post(ctx, "/v1/psp/authorize", map[string]any{
		"card_number": card, "amount": amount, "order_ref": orderRef, "callback_url": callbackURL,
	})
	if err != nil {
		return authResult{}, err
	}
	id, _ := m["auth_id"].(string)
	last4, _ := m["card_last4"].(string)
	return authResult{AuthID: id, Last4: last4}, nil
}

func (h *httpPSP) Capture(ctx context.Context, authID string, amount money, callbackURL string) (string, error) {
	m, _, err := h.post(ctx, "/v1/psp/capture", map[string]any{"auth_id": authID, "amount": amount, "callback_url": callbackURL})
	if err != nil {
		return "", err
	}
	id, _ := m["capture_id"].(string)
	return id, nil
}

func (h *httpPSP) Refund(ctx context.Context, captureID string, amount money, callbackURL string) (string, error) {
	m, _, err := h.post(ctx, "/v1/psp/refund", map[string]any{"capture_id": captureID, "amount": amount, "callback_url": callbackURL})
	if err != nil {
		return "", err
	}
	id, _ := m["refund_id"].(string)
	return id, nil
}

// --- resilient wrapper: bounded retry + circuit breaker ---------------------

// codeCircuitOpen is returned when the breaker is open (fast-fail; retryable).
// codePSPTimeout / codePSPDeclined map the adapter errors to the 02 §2 wire.

// resilientPSP wraps a PSP with a bounded retry on retryable (timeout/unavailable)
// errors and a per-op circuit breaker: after `threshold` consecutive retryable
// failures the breaker OPENS for `cooldown` (fast-failing further calls), then
// half-opens to probe. Declines are NOT retried and do NOT trip the breaker
// (they are a normal issuer outcome, not a PSP fault). Uses the injected clock.
type resilientPSP struct {
	inner     PSP
	clock     Clock
	maxTries  int
	threshold int
	cooldown  time.Duration

	mu       sync.Mutex
	failures int
	openedAt time.Time
	open     bool
}

func newResilientPSP(inner PSP, clock Clock) *resilientPSP {
	return &resilientPSP{inner: inner, clock: clock, maxTries: 3, threshold: 5, cooldown: 30 * time.Second}
}

// breakerOpen reports whether the breaker is currently open (after honouring the
// cooldown half-open transition). Caller holds no lock.
func (r *resilientPSP) breakerOpen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open && r.clock.Now().Sub(r.openedAt) >= r.cooldown {
		r.open = false // half-open: allow one probe
		r.failures = 0
	}
	return r.open
}

func (r *resilientPSP) recordSuccess() {
	r.mu.Lock()
	r.failures = 0
	r.open = false
	r.mu.Unlock()
}

func (r *resilientPSP) recordFailure() {
	r.mu.Lock()
	r.failures++
	if r.failures >= r.threshold {
		r.open = true
		r.openedAt = r.clock.Now()
	}
	r.mu.Unlock()
}

// CircuitOpen reports the breaker state (diagnostics/metrics/tests).
func (r *resilientPSP) CircuitOpen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.open
}

func (r *resilientPSP) Authorize(ctx context.Context, orderRef, card string, amount money, cb string) (authResult, error) {
	if r.breakerOpen() {
		return authResult{}, &pspError{pspUnavailable, "circuit open"}
	}
	var last error
	for try := 0; try < r.maxTries; try++ {
		res, err := r.inner.Authorize(ctx, orderRef, card, amount, cb)
		if err == nil {
			r.recordSuccess()
			return res, nil
		}
		if declined(err) {
			r.recordSuccess() // a decline is a valid answer, not a fault
			return authResult{}, err
		}
		if !retryable(err) {
			r.recordFailure()
			return authResult{}, err
		}
		last = err
		r.recordFailure()
		if r.breakerOpen() {
			break
		}
	}
	return authResult{}, last
}

func (r *resilientPSP) Capture(ctx context.Context, authID string, amount money, cb string) (string, error) {
	if r.breakerOpen() {
		return "", &pspError{pspUnavailable, "circuit open"}
	}
	var last error
	for try := 0; try < r.maxTries; try++ {
		id, err := r.inner.Capture(ctx, authID, amount, cb)
		if err == nil {
			r.recordSuccess()
			return id, nil
		}
		if !retryable(err) {
			r.recordFailure()
			return "", err
		}
		last = err
		r.recordFailure()
	}
	return "", last
}

func (r *resilientPSP) Refund(ctx context.Context, captureID string, amount money, cb string) (string, error) {
	if r.breakerOpen() {
		return "", &pspError{pspUnavailable, "circuit open"}
	}
	var last error
	for try := 0; try < r.maxTries; try++ {
		id, err := r.inner.Refund(ctx, captureID, amount, cb)
		if err == nil {
			r.recordSuccess()
			return id, nil
		}
		if !retryable(err) {
			r.recordFailure()
			return "", err
		}
		last = err
		r.recordFailure()
	}
	return "", last
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
