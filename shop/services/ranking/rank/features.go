package rank

import (
	"math"
	"sync"
)

// features is the per-merchant feature vector the re-ranking model reads. It is
// populated ENTIRELY from events (the `ranking.signal` topic — see consumer.go):
// impressions, clicks and completed orders stream in as signals and the store
// maintains running aggregates. The re-rank score (scorer.go) is a function of
// these features plus the retrieval-time rating/distance, so a store's position
// in the feed reflects observed behaviour, not just its static rating.
type features struct {
	Impressions int64
	Clicks      int64
	Orders      float64 // weighted order volume (popularity)
}

// popularity is the model's popularity signal: log1p of weighted order volume so
// a store with 1000 orders does not dwarf one with 100 linearly (diminishing returns).
func (f features) popularity() float64 { return math.Log1p(f.Orders) }

// ctr is the click-through rate (clicks / impressions), a conversion signal. It
// is 0 until the store has been shown at least once.
func (f features) ctr() float64 {
	if f.Impressions <= 0 {
		return 0
	}
	return float64(f.Clicks) / float64(f.Impressions)
}

// FeatureStore is the event-fed feature store: a concurrent per-merchant map of
// feature vectors. Writes come from the signal consumer (exactly-once via the
// inbox); reads come from the scorer on the hot path. It is the sandbox model of
// the production online feature store (Redis/feature-platform) — the SHAPE
// (event-sourced running aggregates, read on the ranking hot path) is faithful;
// only the backing store is in-process (disclosed in VERIFICATION.md §V-T5).
type FeatureStore struct {
	mu sync.RWMutex
	m  map[string]*features
}

// NewFeatureStore builds an empty feature store.
func NewFeatureStore() *FeatureStore { return &FeatureStore{m: map[string]*features{}} }

// SignalType enumerates the observed behaviours a signal event carries.
const (
	SignalImpression = "impression"
	SignalClick      = "click"
	SignalOrder      = "order"
)

// Apply folds one signal into a merchant's feature vector. weight defaults to 1
// when non-positive. It is called by the consumer inside the inbox's
// exactly-once effect, so a redelivered signal never double-counts.
func (s *FeatureStore) Apply(merchantID, signalType string, weight float64) {
	if merchantID == "" {
		return
	}
	if weight <= 0 {
		weight = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.m[merchantID]
	if f == nil {
		f = &features{}
		s.m[merchantID] = f
	}
	switch signalType {
	case SignalImpression:
		f.Impressions += int64(weight)
	case SignalClick:
		f.Clicks += int64(weight)
	case SignalOrder:
		f.Orders += weight
	}
}

// get returns a snapshot copy of a merchant's features (zero value if unknown).
func (s *FeatureStore) get(merchantID string) features {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if f := s.m[merchantID]; f != nil {
		return *f
	}
	return features{}
}

// Merchants is the number of merchants with at least one recorded feature
// (audit/stats).
func (s *FeatureStore) Merchants() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// Popularity exposes a merchant's popularity signal for stats/debug endpoints.
func (s *FeatureStore) Popularity(merchantID string) float64 { return s.get(merchantID).popularity() }
