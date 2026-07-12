package main

import (
	"sync"
	"time"
)

// admission.go — D7 KITCHEN-CAPACITY ADMISSION TOKENS. A merchant can accept at
// most `capacity` orders per sliding window `window` (default 30 accepts / 10
// min, merchant-tunable). When the kitchen is at capacity the system does NOT
// fail checkout — instead the quoted prep ETA inflates and a "busy" badge shows,
// and further accepts are DEFERRED (the order stays PENDING in the queue). This
// is a back-pressure control, not an error path: the 50× flash-sale test asserts
// zero checkout 5xx and an accept rate within ±5% of the configured capacity.
//
// The token ledger is a sliding window of grant timestamps per merchant, read
// against the injected clock (so the flash-sale test freezes the window). In
// production the ledger lives in Redis (per-cell); here it is in-process
// (disclosed in VERIFICATION §V-T11) — the admission ARITHMETIC is identical.

// DefaultCapacity / DefaultWindow are the D7 defaults (30 accepts / 10 min).
const (
	DefaultCapacity = 30
	DefaultWindow   = 10 * time.Minute
)

// baseETAMinutes is the quoted prep ETA when the kitchen is idle; each order of
// backlog beyond the current window's remaining capacity adds
// etaInflationPerOrder to the quote (the "busy => longer ETA" signal).
const (
	baseETAMinutes       = 15
	etaInflationPerOrder = 1
	maxETAMinutes        = 90
)

type capacityCfg struct {
	capacity int
	window   time.Duration
}

// Admission is the per-merchant kitchen-capacity controller.
type Admission struct {
	mu     sync.Mutex
	cfg    map[string]capacityCfg
	grants map[string][]time.Time // sliding-window accept grant timestamps
}

func newAdmission() *Admission {
	return &Admission{cfg: map[string]capacityCfg{}, grants: map[string][]time.Time{}}
}

func (a *Admission) cfgFor(merchantID string) capacityCfg {
	c, ok := a.cfg[merchantID]
	if !ok {
		return capacityCfg{capacity: DefaultCapacity, window: DefaultWindow}
	}
	return c
}

// SetCapacity tunes a merchant's kitchen capacity (merchant-tunable). A capacity
// of 0 or negative is rejected by the caller; window defaults to 10 min if unset.
func (a *Admission) SetCapacity(merchantID string, capacity int, window time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if window <= 0 {
		window = DefaultWindow
	}
	a.cfg[merchantID] = capacityCfg{capacity: capacity, window: window}
}

// prune drops grant timestamps older than the window (caller holds the lock).
func (a *Admission) prune(merchantID string, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	src := a.grants[merchantID]
	kept := src[:0:0]
	for _, t := range src {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	a.grants[merchantID] = kept
	return kept
}

// TryAccept consumes one admission token for a merchant at `now`. It returns
// granted=true (a token was available, the accept may proceed to drive the saga)
// or granted=false (kitchen at capacity — defer with a busy badge). Atomic under
// the lock so a burst of concurrent accepts admits at most `capacity` per window.
func (a *Admission) TryAccept(merchantID string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := a.cfgFor(merchantID)
	kept := a.prune(merchantID, now, cfg.window)
	if len(kept) >= cfg.capacity {
		return false
	}
	a.grants[merchantID] = append(kept, now)
	return true
}

// Refund releases the most recent token for a merchant (used when the downstream
// saga accept did NOT actually apply — e.g. the order was not PAID — so the
// admitted rate reflects real accepts, keeping accept-rate = capacity ± 5%).
func (a *Admission) Refund(merchantID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.grants[merchantID]
	if len(g) > 0 {
		a.grants[merchantID] = g[:len(g)-1]
	}
}

// AdmissionStatus is the merchant capacity/busy-badge view.
type AdmissionStatus struct {
	MerchantID       string `json:"merchant_id"`
	AcceptsPerWindow int    `json:"accepts_per_window"`
	WindowMinutes    int    `json:"window_minutes"`
	Used             int    `json:"used"`
	Remaining        int    `json:"remaining"`
	Busy             bool   `json:"busy"`
	PrepETAMinutes   int    `json:"prep_eta_minutes"`
	PendingCount     int    `json:"pending_count"`
}

// Status computes the current admission state + the busy badge + the inflated
// prep ETA for a merchant, given the pending-order backlog from the read model.
func (a *Admission) Status(merchantID string, now time.Time, pending int) AdmissionStatus {
	a.mu.Lock()
	cfg := a.cfgFor(merchantID)
	kept := a.prune(merchantID, now, cfg.window)
	used := len(kept)
	a.mu.Unlock()

	remaining := cfg.capacity - used
	if remaining < 0 {
		remaining = 0
	}
	busy := remaining == 0
	return AdmissionStatus{
		MerchantID:       merchantID,
		AcceptsPerWindow: cfg.capacity,
		WindowMinutes:    int(cfg.window / time.Minute),
		Used:             used,
		Remaining:        remaining,
		Busy:             busy,
		PrepETAMinutes:   prepETA(remaining, pending),
		PendingCount:     pending,
	}
}

// prepETA inflates the quoted prep time by the backlog that cannot be admitted in
// the current window (pending beyond the remaining tokens). Idle kitchen ⇒ base.
func prepETA(remaining, pending int) int {
	backlog := pending - remaining
	if backlog < 0 {
		backlog = 0
	}
	eta := baseETAMinutes + backlog*etaInflationPerOrder
	if eta > maxETAMinutes {
		eta = maxETAMinutes
	}
	return eta
}
