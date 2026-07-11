package rank

import (
	"context"
	"errors"
	"sync/atomic"
)

// Candidate is one store in the retrieval set (the top-500 the search service
// returns for a browse point). Ranking re-orders these to the top-50. The shape
// is the search browse feed item (02 §4.2): re-ranking changes ORDER only, never
// the item content, so the feed the customer sees is identical field-for-field to
// what search produced — just in a better order.
type Candidate struct {
	StoreID     string  `json:"store_id"`
	Name        string  `json:"name"`
	Rating      float64 `json:"rating"`
	DistanceM   int     `json:"distance_m"`
	Open        bool    `json:"open"`
	DeliveryFee Money   `json:"delivery_fee"`
	ETAMinutes  int     `json:"eta_minutes"`
}

// Money is integer minor units + ISO currency (02 §1), carried through unchanged.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// Scorer assigns a relevance score to each candidate; the ranker sorts by score
// descending. Two implementations: mlScorer (the feature-weighted "model") and
// staticScorer (the retrieval-order fallback). Score MAY return an error to model
// a model-serving outage — the ranker catches it and falls back to static
// (auto-fallback, breaker.go).
type Scorer interface {
	Score(ctx context.Context, cands []Candidate) ([]float64, error)
	// Name identifies the scorer for stats/telemetry ("ml" | "static").
	Name() string
}

// ErrModelUnavailable is returned by a scorer whose model-serving path is down.
// It is the signal the auto-fallback breaker trips on.
var ErrModelUnavailable = errors.New("ranking model unavailable")

// ModelWeights are the linear coefficients of the stand-in "model". THIS IS NOT A
// TRAINED ML MODEL: it is a deterministic feature-weighted scoring function that
// stands in for the served model so the slice is fully testable without training
// infrastructure (disclosed in VERIFICATION.md §V-T5 and docs/runbooks/ranking.md).
// The model-deploy pipeline that would ship real weights is documented in the
// runbook; swapping these coefficients for trained ones is the only change.
type ModelWeights struct {
	Rating     float64 // higher rating ranks up
	Popularity float64 // log1p(order volume) — the event-fed popularity signal
	CTR        float64 // clicks/impressions — the event-fed conversion signal
	Distance   float64 // per-km penalty (closer ranks up)
}

// DefaultWeights are the shipped coefficients. Popularity and CTR are event-fed,
// so a store's feed position moves with observed behaviour — that is what makes
// the ML order differ from the static (rating,distance) order.
var DefaultWeights = ModelWeights{Rating: 1.0, Popularity: 0.8, CTR: 2.0, Distance: 0.15}

// mlScorer is the served-model stand-in: score = Σ weight·feature over the
// retrieval-time rating/distance plus the event-fed popularity/CTR features.
type mlScorer struct {
	weights ModelWeights
	feats   *FeatureStore
	// down, when set true, forces every Score call to return ErrModelUnavailable —
	// the deterministic, race-safe hook the fallback test uses to inject a model
	// outage. Shared with the Ranker via SetModelDown; false in production.
	down *atomic.Bool
}

func newMLScorer(w ModelWeights, feats *FeatureStore, down *atomic.Bool) *mlScorer {
	return &mlScorer{weights: w, feats: feats, down: down}
}

func (m *mlScorer) Name() string { return "ml" }

func (m *mlScorer) Score(ctx context.Context, cands []Candidate) ([]float64, error) {
	if m.down != nil && m.down.Load() {
		return nil, ErrModelUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]float64, len(cands))
	for i, c := range cands {
		f := m.feats.get(c.StoreID)
		km := float64(c.DistanceM) / 1000.0
		out[i] = m.weights.Rating*c.Rating +
			m.weights.Popularity*f.popularity() +
			m.weights.CTR*f.ctr() -
			m.weights.Distance*km
	}
	return out, nil
}

// staticScorer reproduces the retrieval order deterministically: it scores by the
// same (rating desc, then distance asc) key search already sorted by, so a static
// re-rank is a stable no-op on the retrieval order. This is the fallback path AND
// the shed-ladder L1 behaviour (D12/D17): under overload or a model outage the
// feed serves this cheap deterministic order.
type staticScorer struct{}

func (staticScorer) Name() string { return "static" }

func (staticScorer) Score(_ context.Context, cands []Candidate) ([]float64, error) {
	out := make([]float64, len(cands))
	for i, c := range cands {
		// rating dominates; distance breaks ties (closer = higher). Scaled so the
		// distance term can never overtake a rating difference of 0.1.
		out[i] = c.Rating*1e6 - float64(c.DistanceM)
	}
	return out, nil
}
