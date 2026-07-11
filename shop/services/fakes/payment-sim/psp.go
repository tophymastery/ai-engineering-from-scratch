package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// PSP is the scriptable payment fake's core (03 §5). Everything probabilistic —
// auth/capture/refund ids, reported latencies, webhook event ids and their
// ORDERING — is driven by a single seeded *rand.Rand and a single deterministic
// clock, both guarded by mu. Given the same seed and the same (sequential)
// request sequence, every byte of output is reproduced across reruns. The
// webhook dispatcher is a single FIFO goroutine, so event delivery order equals
// enqueue order equals request order — deterministic by construction.
type PSP struct {
	mu       sync.Mutex
	rnd      *rand.Rand
	seq      int64     // monotonic webhook sequence + logical event clock
	clock    time.Time // deterministic wall-clock stand-in
	step     time.Duration
	timeout  time.Duration // how long an "...0044" card sleeps before 504
	auths    map[string]*authRec
	captures map[string]*captureRec
	settle   map[string][]*captureRec // settlement_date -> captures (append order)

	webhookCh chan webhookJob
	client    *http.Client
	wg        sync.WaitGroup
	closeOnce sync.Once
}

type authRec struct {
	authID   string
	amount   Money
	last4    string
	orderRef string
}

type captureRec struct {
	captureID  string
	authID     string
	amount     Money
	settleDate string
	capturedAt string
}

// Money is the 02 §1 integer-minor-unit money value.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type webhookJob struct {
	url  string
	body []byte
}

// Config parameterises the fake without touching wall time or crypto/rand.
type Config struct {
	Seed    int64
	T0      time.Time     // deterministic clock start
	Step    time.Duration // clock advance per stamped event
	Timeout time.Duration // "...0044" card sleep-before-504
	Client  *http.Client  // webhook delivery client (nil => default, 2s timeout)
}

// NewPSP builds a PSP and starts its single ordered webhook dispatcher.
func NewPSP(cfg Config) *PSP {
	if cfg.T0.IsZero() {
		cfg.T0 = time.Date(2026, 7, 11, 2, 15, 0, 0, time.UTC)
	}
	if cfg.Step == 0 {
		cfg.Step = time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 2 * time.Second}
	}
	p := &PSP{
		rnd:       rand.New(rand.NewSource(cfg.Seed)),
		clock:     cfg.T0,
		step:      cfg.Step,
		timeout:   cfg.Timeout,
		auths:     map[string]*authRec{},
		captures:  map[string]*captureRec{},
		settle:    map[string][]*captureRec{},
		webhookCh: make(chan webhookJob, 1024),
		client:    cfg.Client,
	}
	p.wg.Add(1)
	go p.dispatch()
	return p
}

// Close drains and stops the webhook dispatcher. Safe to call once.
func (p *PSP) Close() {
	p.closeOnce.Do(func() {
		close(p.webhookCh)
		p.wg.Wait()
	})
}

// dispatch delivers webhooks strictly in enqueue order (single consumer).
func (p *PSP) dispatch() {
	defer p.wg.Done()
	for job := range p.webhookCh {
		if job.url == "" {
			continue
		}
		req, err := http.NewRequest(http.MethodPost, job.url, bytes.NewReader(job.body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if resp, err := p.client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
}

// --- deterministic primitives (all callers hold p.mu) ---

// stamp returns the next deterministic timestamp and advances the clock.
func (p *PSP) stamp() string {
	t := p.clock
	p.clock = p.clock.Add(p.step)
	return t.UTC().Format(time.RFC3339)
}

func (p *PSP) nextID(kind string) string {
	return fmt.Sprintf("psp_%s_%016x", kind, p.rnd.Uint64())
}

func (p *PSP) latencyMS() int {
	return 50 + p.rnd.Intn(400)
}

// enqueueWebhook builds and queues an event under the lock so seq, event_id and
// occurred_at are deterministic and the queue order matches the request order.
func (p *PSP) enqueueWebhook(url, eventType string, ids map[string]string) {
	p.seq++
	ev := map[string]any{
		"event_id":    fmt.Sprintf("evt_%016x", p.rnd.Uint64()),
		"event_type":  eventType,
		"occurred_at": p.stamp(),
		"seq":         p.seq,
	}
	for k, v := range ids {
		ev[k] = v
	}
	body, _ := json.Marshal(ev)
	// Non-blocking within our generous buffer; a full buffer would only happen
	// far beyond any test/seed workload.
	p.webhookCh <- webhookJob{url: url, body: body}
}

// --- operations (return status, response body, error-envelope-or-nil) ---

type apiErr struct {
	status    int
	code      string
	message   string
	field     string
	reason    string
	retryable bool
}

// Authorize implements the card script: ...0002 declines, ...0044 times out,
// else AUTHORIZED plus an async payment.authorized webhook.
func (p *PSP) Authorize(card string, amount Money, orderRef, callbackURL string) (any, *apiErr) {
	last4 := lastN(card, 4)

	if strings.HasSuffix(card, "0002") {
		return nil, &apiErr{http.StatusPaymentRequired, "PSP_CARD_DECLINED",
			"Card ending 0002 was declined by the issuer.", "card_number", "card_declined", false}
	}
	if strings.HasSuffix(card, "0044") {
		// Deterministic timeout: sleep past the caller's deadline, then 504.
		if p.timeout > 0 {
			time.Sleep(p.timeout)
		}
		return nil, &apiErr{http.StatusGatewayTimeout, "PSP_TIMEOUT",
			"Card ending 0044 timed out at the issuer.", "card_number", "issuer_timeout", true}
	}

	p.mu.Lock()
	authID := p.nextID("auth")
	latency := p.latencyMS()
	at := p.stamp()
	p.auths[authID] = &authRec{authID: authID, amount: amount, last4: last4, orderRef: orderRef}
	p.enqueueWebhook(callbackURL, "payment.authorized", map[string]string{"auth_id": authID})
	p.mu.Unlock()

	return map[string]any{
		"auth_id":       authID,
		"status":        "AUTHORIZED",
		"card_last4":    last4,
		"amount":        amount,
		"latency_ms":    latency,
		"authorized_at": at,
	}, nil
}

// Capture records a capture against a known auth and files it for that day's
// settlement CSV, plus an async payment.captured webhook.
func (p *PSP) Capture(authID string, amount Money, settleDate, callbackURL string) (any, *apiErr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.auths[authID]; !ok {
		return nil, &apiErr{http.StatusNotFound, "PSP_UNKNOWN_AUTH",
			"No authorization with that auth_id.", "auth_id", "unknown", false}
	}
	if settleDate == "" {
		settleDate = p.clock.UTC().Format("2006-01-02")
	}
	capID := p.nextID("cap")
	at := p.stamp()
	rec := &captureRec{captureID: capID, authID: authID, amount: amount, settleDate: settleDate, capturedAt: at}
	p.captures[capID] = rec
	p.settle[settleDate] = append(p.settle[settleDate], rec)
	p.enqueueWebhook(callbackURL, "payment.captured", map[string]string{"capture_id": capID, "auth_id": authID})

	return map[string]any{
		"capture_id":      capID,
		"auth_id":         authID,
		"status":          "CAPTURED",
		"amount":          amount,
		"settlement_date": settleDate,
		"captured_at":     at,
	}, nil
}

// Refund reverses a known capture, plus an async payment.refunded webhook.
func (p *PSP) Refund(captureID string, amount Money, callbackURL string) (any, *apiErr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.captures[captureID]; !ok {
		return nil, &apiErr{http.StatusNotFound, "PSP_UNKNOWN_AUTH",
			"No capture with that capture_id.", "capture_id", "unknown", false}
	}
	refID := p.nextID("ref")
	at := p.stamp()
	p.enqueueWebhook(callbackURL, "payment.refunded", map[string]string{"refund_id": refID, "capture_id": captureID})

	return map[string]any{
		"refund_id":   refID,
		"capture_id":  captureID,
		"status":      "REFUNDED",
		"amount":      amount,
		"refunded_at": at,
	}, nil
}

// SettlementFile renders a deterministic CSV of a day's captures, sorted by
// capture_id so the file is byte-identical for identical capture sets.
func (p *PSP) SettlementFile(date string) string {
	p.mu.Lock()
	recs := append([]*captureRec(nil), p.settle[date]...)
	p.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].captureID < recs[j].captureID })

	var b strings.Builder
	b.WriteString("capture_id,auth_id,amount_minor,currency,captured_at\n")
	for _, r := range recs {
		fmt.Fprintf(&b, "%s,%s,%d,%s,%s\n", r.captureID, r.authID, r.amount.Amount, r.amount.Currency, r.capturedAt)
	}
	return b.String()
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
