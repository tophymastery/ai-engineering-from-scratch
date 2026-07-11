package rank

import (
	"sync"
	"time"
)

// breaker is the auto-fallback circuit for the ML re-rank path. It has two jobs,
// which together give the V-T5 correctness property "ranking outage ⇒ feed
// availability ≥ 99.9% via auto-fallback < 10 s":
//
//  1. AVAILABILITY (request-level): every ML failure the ranker sees is reported
//     here; the ranker serves static for THAT request, so a request never fails
//     because the model is down. Availability is preserved from the very first
//     failure.
//  2. ENGAGEMENT (< 10 s): a health monitor probes the model on a fixed cadence
//     (default every 2 s). After `threshold` consecutive failed probes it OPENS —
//     subsequent Rank calls skip the ML attempt entirely (no wasted model latency)
//     and serve static directly. openedAt−outageStart is the engagement time; with
//     a 2 s cadence and threshold 1 it is ≤ 2 s ≪ 10 s. A subsequent SUCCESSFUL
//     probe CLOSES it again (auto-recovery).
//
// All timing reads the injected Clock, so the fallback test drives the 10 s window
// deterministically by advancing a ManualClock — it never sleeps.
type breaker struct {
	clock     Clock
	interval  time.Duration // probe cadence
	threshold int           // consecutive failed probes to open

	mu         sync.Mutex
	open       bool
	consecFail int
	openedAt   time.Time
	lastChange time.Time
}

func newBreaker(clock Clock, interval time.Duration, threshold int) *breaker {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if threshold <= 0 {
		threshold = 1
	}
	return &breaker{clock: clock, interval: interval, threshold: threshold}
}

// Open reports whether the breaker is currently open (ML path considered down, so
// Rank should serve static without attempting the model).
func (b *breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

// OpenedAt returns when the breaker last opened (zero if never / currently
// closed). The fallback test uses it to measure engagement time.
func (b *breaker) OpenedAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return time.Time{}
	}
	return b.openedAt
}

// probe folds one health-probe result into the breaker state and returns the new
// open state. ok=true is a healthy probe (closes, resets), ok=false is a failed
// probe (counts toward the open threshold).
func (b *breaker) probe(ok bool) bool {
	now := b.clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.consecFail = 0
		if b.open {
			b.open = false
			b.lastChange = now
		}
		return b.open
	}
	b.consecFail++
	if !b.open && b.consecFail >= b.threshold {
		b.open = true
		b.openedAt = now
		b.lastChange = now
	}
	return b.open
}
