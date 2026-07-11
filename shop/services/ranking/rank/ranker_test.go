package rank

import (
	"context"
	"fmt"
	"testing"
)

// candA/candB are two stores at the same distance: B is higher-rated but unknown,
// A is lower-rated but popular (fed from events). Static ranks B first (rating);
// ML ranks A first once its popularity feature is loaded.
func candA() Candidate {
	return Candidate{StoreID: "mer_a_popular", Name: "A", Rating: 4.0, DistanceM: 500, Open: true}
}
func candB() Candidate {
	return Candidate{StoreID: "mer_b_highrated", Name: "B", Rating: 4.5, DistanceM: 500, Open: true}
}

func TestRank_StaticIsRetrievalOrder(t *testing.T) {
	feats := NewFeatureStore()
	r := NewRanker(feats, Options{})
	// Even with A popular, static ignores features: B (rating 4.5) must come first.
	feats.Apply("mer_a_popular", SignalOrder, 50)
	out, usedML := r.Rank(context.Background(), []Candidate{candA(), candB()}, 10, false /* ranking_ml OFF */)
	if usedML {
		t.Fatal("ranking_ml OFF must not use the ML path")
	}
	if out[0].StoreID != "mer_b_highrated" {
		t.Fatalf("static order should rank higher-rated B first, got %s", out[0].StoreID)
	}
}

func TestRank_MLDiffersFromStatic(t *testing.T) {
	feats := NewFeatureStore()
	r := NewRanker(feats, Options{})
	// A accrues popularity from events; B stays unknown.
	feats.Apply("mer_a_popular", SignalOrder, 10)

	cands := []Candidate{candB(), candA()} // retrieval order: B then A (rating)
	static, _ := r.Rank(context.Background(), cands, 10, false)
	ml, usedML := r.Rank(context.Background(), cands, 10, true)

	if !usedML {
		t.Fatal("ranking_ml ON with a healthy model must use the ML path")
	}
	if static[0].StoreID != "mer_b_highrated" {
		t.Fatalf("static: expected B first, got %s", static[0].StoreID)
	}
	if ml[0].StoreID != "mer_a_popular" {
		t.Fatalf("ML: expected popular A promoted to first, got %s (%v)", ml[0].StoreID, ids(ml))
	}
	if static[0].StoreID == ml[0].StoreID {
		t.Fatal("ML and static produced the same top store — flag states are indistinguishable")
	}
}

func TestRank_TopKTruncation(t *testing.T) {
	feats := NewFeatureStore()
	r := NewRanker(feats, Options{})
	cands := make([]Candidate, 500)
	for i := range cands {
		cands[i] = Candidate{StoreID: fmt.Sprintf("mer_%03d", i), Rating: float64(i % 5), DistanceM: i, Open: true}
	}
	out, _ := r.Rank(context.Background(), cands, DefaultTopK, true)
	if len(out) != DefaultTopK {
		t.Fatalf("top-500 -> top-K: expected %d results, got %d", DefaultTopK, len(out))
	}
}

func TestRank_Deterministic(t *testing.T) {
	feats := NewFeatureStore()
	feats.Apply("mer_tie_2", SignalOrder, 3)
	feats.Apply("mer_tie_1", SignalOrder, 3)
	r := NewRanker(feats, Options{})
	cands := []Candidate{
		{StoreID: "mer_tie_2", Rating: 4.0, DistanceM: 100, Open: true},
		{StoreID: "mer_tie_1", Rating: 4.0, DistanceM: 100, Open: true},
	}
	first, _ := r.Rank(context.Background(), cands, 10, true)
	for i := 0; i < 20; i++ {
		out, _ := r.Rank(context.Background(), cands, 10, true)
		if ids(out) != ids(first) {
			t.Fatalf("non-deterministic re-rank: %v vs %v", ids(out), ids(first))
		}
	}
	// Equal-score tie broken by StoreID asc (determinism, 01 §6).
	if first[0].StoreID != "mer_tie_1" {
		t.Fatalf("tie should break by StoreID asc, got %s first", first[0].StoreID)
	}
}

func ids(cs []Candidate) string {
	s := ""
	for _, c := range cs {
		s += c.StoreID + ";"
	}
	return s
}
