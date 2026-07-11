package rank

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAutoFallback_EngagesWithin10s is the V-T5 timing property: after a model
// outage, the auto-fallback breaker OPENS in < 10 s. It is driven entirely by the
// injected ManualClock — the health monitor's 2 s probe cadence is simulated by
// advancing time and calling HealthProbe; NO wall-clock sleep is used (doc 01 §6 /
// 03 §1). Engagement time = openedAt − outageStart.
func TestAutoFallback_EngagesWithin10s(t *testing.T) {
	clk := NewManualClock(time.Unix(1_700_000_000, 0))
	feats := NewFeatureStore()
	r := NewRanker(feats, Options{Clock: clk, ProbeInterval: 2 * time.Second, ProbeFailOpen: 1})

	// Healthy to start: a probe keeps the breaker closed.
	if r.HealthProbe(context.Background()); r.FallbackEngaged() {
		t.Fatal("breaker should start closed on a healthy model")
	}

	// Inject the model outage.
	outageStart := clk.Now()
	r.SetModelDown(true)

	// Simulate the 2 s health-monitor cadence by advancing the clock and probing,
	// for up to 10 s. Assert the breaker engages within the window.
	engaged := false
	var engagedAt time.Time
	for step := 0; step < 5; step++ { // 5 × 2 s = 10 s
		clk.Advance(2 * time.Second)
		if r.HealthProbe(context.Background()) {
			engaged = true
			engagedAt = r.OpenedAt()
			break
		}
	}
	if !engaged {
		t.Fatal("auto-fallback never engaged within 10 s of the model outage")
	}
	dt := engagedAt.Sub(outageStart)
	if dt <= 0 || dt >= 10*time.Second {
		t.Fatalf("auto-fallback engaged at %v after outage — must be > 0 and < 10 s", dt)
	}
	t.Logf("auto-fallback engaged %v after model outage (budget < 10 s)", dt)

	// With the breaker open, a Rank call serves static WITHOUT attempting the model.
	out, usedML := r.Rank(context.Background(), []Candidate{candB(), candA()}, 10, true)
	if usedML {
		t.Fatal("with the breaker open, Rank must serve static, not ML")
	}
	if out[0].StoreID != "mer_b_highrated" {
		t.Fatalf("fallback served wrong order: %v", ids(out))
	}
}

// TestAutoFallback_AvailabilityAcrossOutage is the V-T5 availability property: a
// request stream that SPANS a model outage keeps serving the feed (static order)
// at ≥ 99.9% availability — a request never fails because the model is down. Real
// measurement over a concurrent stream (runs under -race).
func TestAutoFallback_AvailabilityAcrossOutage(t *testing.T) {
	feats := NewFeatureStore()
	feats.Apply("mer_a_popular", SignalOrder, 10)
	r := NewRanker(feats, Options{ProbeInterval: 2 * time.Second, ProbeFailOpen: 1})

	cands := []Candidate{candB(), candA()}
	const total = 5000
	// The outage begins partway through the stream and never recovers within it.
	const outageAt = 1000

	var mu sync.Mutex
	var served, ok int
	var wg sync.WaitGroup
	// A stream of concurrent browse requests spanning the outage boundary.
	for i := 0; i < total; i++ {
		if i == outageAt {
			r.SetModelDown(true) // model goes down mid-stream
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _ := r.Rank(context.Background(), cands, 10, true /* ranking_ml ON */)
			mu.Lock()
			served++
			// A request is "available" if it returned a non-empty, valid ordering.
			if len(out) == len(cands) && out[0].StoreID != "" {
				ok++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	avail := float64(ok) / float64(served)
	t.Logf("feed availability across model outage: %.4f%% (%d/%d requests served a valid feed)", avail*100, ok, served)
	if avail < 0.999 {
		t.Fatalf("feed availability %.4f%% < 99.9%% during the model outage (auto-fallback FAILED)", avail*100)
	}
	// Every request that hit the outage fell back to STATIC order (correctness of
	// the degraded feed): higher-rated B first.
	postOutage, usedML := r.Rank(context.Background(), cands, 10, true)
	if usedML {
		t.Fatal("during the outage Rank must not report ML use")
	}
	if postOutage[0].StoreID != "mer_b_highrated" {
		t.Fatalf("degraded feed should be static order (B first), got %v", ids(postOutage))
	}
}

// TestAutoFallback_Recovery proves the breaker auto-closes and ML resumes once the
// model is healthy again (a successful probe), so the fallback is not sticky.
func TestAutoFallback_Recovery(t *testing.T) {
	clk := NewManualClock(time.Unix(1_700_000_000, 0))
	feats := NewFeatureStore()
	feats.Apply("mer_a_popular", SignalOrder, 10)
	r := NewRanker(feats, Options{Clock: clk, ProbeInterval: 2 * time.Second, ProbeFailOpen: 1})

	r.SetModelDown(true)
	clk.Advance(2 * time.Second)
	if !r.HealthProbe(context.Background()) {
		t.Fatal("breaker should be open after a failed probe")
	}

	// Model recovers; the next probe closes the breaker and ML resumes.
	r.SetModelDown(false)
	clk.Advance(2 * time.Second)
	if r.HealthProbe(context.Background()) {
		t.Fatal("breaker should close after a healthy probe")
	}
	_, usedML := r.Rank(context.Background(), []Candidate{candB(), candA()}, 10, true)
	if !usedML {
		t.Fatal("ML should resume after recovery")
	}
}
