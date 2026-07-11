package rank

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTopK is the re-rank output size — D17's "top 50".
const DefaultTopK = 50

// Options configure a Ranker.
type Options struct {
	Weights       ModelWeights
	Clock         Clock
	ProbeInterval time.Duration // health-probe cadence (default 2s)
	ProbeFailOpen int           // consecutive failed probes to open the breaker (default 1)
}

// Ranker re-ranks a retrieval set (top-500) down to the top-K (50). It holds the
// ML scorer (feature-weighted, event-fed), the static scorer (fallback), and the
// auto-fallback breaker. Which path a Rank call takes:
//
//	mlEnabled == false            → static (the `ranking_ml` flag is OFF)
//	breaker.Open()                → static (auto-fallback engaged after an outage)
//	ml scorer returns an error    → static for THIS request + report to the breaker
//	otherwise                     → ML re-rank
//
// Every branch returns a fully-ranked feed, so a model outage degrades ORDER
// (ML→static) but never AVAILABILITY.
type Ranker struct {
	ml      *mlScorer
	static  staticScorer
	feats   *FeatureStore
	breaker *breaker
	clock   Clock
	down    *atomic.Bool // deterministic model-outage switch (shared with ml scorer)

	// telemetry
	mu           sync.Mutex
	reRankLat    []time.Duration
	served       int64
	servedStatic int64
	servedML     int64
	mlErrors     int64
}

// NewRanker builds a Ranker over a feature store.
func NewRanker(feats *FeatureStore, opt Options) *Ranker {
	if opt.Clock == nil {
		opt.Clock = SystemClock{}
	}
	if opt.Weights == (ModelWeights{}) {
		opt.Weights = DefaultWeights
	}
	down := &atomic.Bool{}
	return &Ranker{
		feats:   feats,
		static:  staticScorer{},
		clock:   opt.Clock,
		breaker: newBreaker(opt.Clock, opt.ProbeInterval, opt.ProbeFailOpen),
		down:    down,
		ml:      newMLScorer(opt.Weights, feats, down),
	}
}

// SetModelDown injects (or clears) a model-serving outage deterministically. With
// down=true the ML scorer returns ErrModelUnavailable on every call and every
// health probe fails — the exact production failure the auto-fallback handles.
func (r *Ranker) SetModelDown(down bool) { r.down.Store(down) }

// Rank re-ranks candidates to at most k using the ML path when eligible, else the
// static fallback. mlEnabled is the per-request `ranking_ml` flag value. It always
// returns a valid ordering (the availability guarantee). The bool reports whether
// the ML path actually produced the result (false = static/fallback).
func (r *Ranker) Rank(ctx context.Context, cands []Candidate, k int, mlEnabled bool) ([]Candidate, bool) {
	if k <= 0 {
		k = DefaultTopK
	}
	start := r.clock.Now()
	usedML := false

	scorer := Scorer(r.static)
	if mlEnabled && !r.breaker.Open() {
		scores, err := r.ml.Score(ctx, cands)
		if err != nil {
			// Model outage on the hot path: fall back to static for this request and
			// tell the breaker (so it can trip and stop wasting model latency).
			r.breaker.probe(false)
			atomic.AddInt64(&r.mlErrors, 1)
		} else {
			out := r.sortByScore(cands, scores, k)
			usedML = true
			r.record(start, true)
			return out, usedML
		}
	}
	scores, _ := scorer.Score(ctx, cands) // static never errors
	out := r.sortByScore(cands, scores, k)
	r.record(start, false)
	return out, usedML
}

// sortByScore returns candidates sorted by score desc, StoreID asc as a stable
// tiebreak (determinism, doc 01 §6), truncated to k.
func (r *Ranker) sortByScore(cands []Candidate, scores []float64, k int) []Candidate {
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if scores[ia] != scores[ib] {
			return scores[ia] > scores[ib]
		}
		return cands[ia].StoreID < cands[ib].StoreID
	})
	n := k
	if n > len(idx) {
		n = len(idx)
	}
	out := make([]Candidate, n)
	for i := 0; i < n; i++ {
		out[i] = cands[idx[i]]
	}
	return out
}

// HealthProbe runs one model health probe and folds the result into the breaker.
// Production runs it on a ticker (RunHealthMonitor); tests call it directly while
// advancing a ManualClock. Returns whether the breaker is open after the probe.
func (r *Ranker) HealthProbe(ctx context.Context) bool {
	_, err := r.ml.Score(ctx, probeCandidates)
	return r.breaker.probe(err == nil)
}

// probeCandidates is a fixed one-item synthetic set the health probe scores — the
// probe exercises the real model path so it detects the real outage.
var probeCandidates = []Candidate{{StoreID: "__probe__", Rating: 5, DistanceM: 0, Open: true}}

// RunHealthMonitor probes the model every ProbeInterval until ctx is cancelled.
// The auto-fallback engagement (< 10 s) is a property of this cadence.
func (r *Ranker) RunHealthMonitor(ctx context.Context) {
	interval := r.breaker.interval
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.HealthProbe(ctx)
		}
	}
}

// FallbackEngaged reports whether the auto-fallback breaker is currently open.
func (r *Ranker) FallbackEngaged() bool { return r.breaker.Open() }

// OpenedAt is when the breaker last opened (for engagement-time measurement).
func (r *Ranker) OpenedAt() time.Time { return r.breaker.OpenedAt() }

func (r *Ranker) record(start time.Time, usedML bool) {
	lat := r.clock.Now().Sub(start)
	r.mu.Lock()
	r.reRankLat = append(r.reRankLat, lat)
	r.mu.Unlock()
	atomic.AddInt64(&r.served, 1)
	if usedML {
		atomic.AddInt64(&r.servedML, 1)
	} else {
		atomic.AddInt64(&r.servedStatic, 1)
	}
}

// Stats is a telemetry snapshot for the /v1/rank/stats endpoint and tests.
type Stats struct {
	Served       int64  `json:"served"`
	ServedML     int64  `json:"served_ml"`
	ServedStatic int64  `json:"served_static"`
	MLErrors     int64  `json:"ml_errors"`
	Merchants    int    `json:"feature_merchants"`
	Fallback     bool   `json:"fallback_engaged"`
	Mode         string `json:"active_scorer"`
}

// Stats returns a snapshot.
func (r *Ranker) Stats() Stats {
	mode := "ml"
	if r.breaker.Open() {
		mode = "static"
	}
	return Stats{
		Served:       atomic.LoadInt64(&r.served),
		ServedML:     atomic.LoadInt64(&r.servedML),
		ServedStatic: atomic.LoadInt64(&r.servedStatic),
		MLErrors:     atomic.LoadInt64(&r.mlErrors),
		Merchants:    r.feats.Merchants(),
		Fallback:     r.breaker.Open(),
		Mode:         mode,
	}
}

// ReRankP99 is the measured p99 of in-process re-rank latency (the < 50 ms SLO).
func (r *Ranker) ReRankP99() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return percentileDur(r.reRankLat, 99)
}

func percentileDur(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(samples))
	copy(cp, samples)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(p / 100 * float64(len(cp)-1))
	return cp[idx]
}
