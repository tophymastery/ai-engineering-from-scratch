package index

import (
	"sync"
	"time"
)

// --- rating debounce (D17) ---
//
// D17: "Rating aggregates debounced (≤1 doc update / merchant / 5 min)." A
// merchant getting a burst of new ratings (a lunch rush, a review-bomb, a
// celebrity merchant) would otherwise rewrite its search document on every single
// rating event, hammering the ingest pipeline for no benefit — the rating
// aggregate barely moves per event. The debouncer coalesces a flood of rating
// updates for one merchant into at most one index write per DebounceWindow,
// keeping the LATEST aggregate. The window is driven by the injected Clock so the
// property is tested by ADVANCING time, never sleeping.

// DebounceWindow is the D17 rating-debounce window: at most one index write per
// merchant per 5 minutes.
const DebounceWindow = 5 * time.Minute

// ratingDebouncer decides, per merchant, whether an incoming rating update is
// allowed to write the search index now or must be coalesced. It holds the latest
// coalesced aggregate so the eventual write carries the freshest value.
type ratingDebouncer struct {
	window time.Duration
	clock  Clock

	mu          sync.Mutex
	lastWriteAt map[string]time.Time // merchant_id -> last index-write time
	pending     map[string]ratingAgg // merchant_id -> latest coalesced aggregate
}

// ratingAgg is a merchant's rating aggregate as carried by rating.updated.
type ratingAgg struct {
	Rating  float64
	Count   int64
	Version int64
}

func newRatingDebouncer(clock Clock, window time.Duration) *ratingDebouncer {
	return &ratingDebouncer{
		window:      window,
		clock:       clock,
		lastWriteAt: map[string]time.Time{},
		pending:     map[string]ratingAgg{},
	}
}

// Offer records a rating update for a merchant and reports whether it should be
// written to the index NOW. It returns (agg, true) when the merchant has had no
// index write within the window (write allowed) — and records this as the new
// write time; otherwise it coalesces the update into `pending` and returns
// (_, false). Last-write-wins by version: a stale (lower-version) update never
// overwrites a fresher pending one.
func (d *ratingDebouncer) Offer(merchantID string, agg ratingAgg) (ratingAgg, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()

	// Coalesce into pending (keep the freshest by version).
	if cur, ok := d.pending[merchantID]; !ok || agg.Version >= cur.Version {
		d.pending[merchantID] = agg
	}

	last, seen := d.lastWriteAt[merchantID]
	if seen && now.Sub(last) < d.window {
		return ratingAgg{}, false // within the window — debounced
	}
	// Window elapsed (or first ever): write the freshest coalesced aggregate.
	out := d.pending[merchantID]
	d.lastWriteAt[merchantID] = now
	delete(d.pending, merchantID)
	return out, true
}

// Flush returns any merchants whose window has elapsed and that still have a
// coalesced pending aggregate waiting to be written, marking them written now.
// A background ticker calls this so the LAST update in a burst is not stranded
// after the window passes with no further events. Returns merchant_id -> agg.
func (d *ratingDebouncer) Flush() map[string]ratingAgg {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()
	out := map[string]ratingAgg{}
	for mid, agg := range d.pending {
		last, seen := d.lastWriteAt[mid]
		if !seen || now.Sub(last) >= d.window {
			out[mid] = agg
			d.lastWriteAt[mid] = now
			delete(d.pending, mid)
		}
	}
	return out
}
