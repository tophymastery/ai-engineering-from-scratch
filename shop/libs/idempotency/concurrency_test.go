package idempotency

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

const stormN = 100

// Criterion 1: 100 concurrent same-key requests ⇒ exactly 1 effect + 99 replays.
func TestStormExactlyOnce(t *testing.T) {
	for _, b := range allBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			store, sink := b.fresh(t)
			m := New(store, NewMemCache())
			key := "idem_storm"
			hash := RequestHash("POST", "/kv", []byte(`{"k":"v"}`))

			var fresh, replay, other int64
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < stormN; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					out, err := m.Do(context.Background(), key, hash, sink.business)
					switch {
					case err != nil:
						atomic.AddInt64(&other, 1)
						t.Errorf("unexpected error: %v", err)
					case out.Replayed:
						atomic.AddInt64(&replay, 1)
					default:
						atomic.AddInt64(&fresh, 1)
					}
				}()
			}
			close(start)
			wg.Wait()

			if got := sink.count(t); got != 1 {
				t.Fatalf("[%s] effects=%d want 1", b.name, got)
			}
			if fresh != 1 || replay != stormN-1 || other != 0 {
				t.Fatalf("[%s] fresh=%d replay=%d other=%d want 1/%d/0", b.name, fresh, replay, other, stormN-1)
			}
			t.Logf("[%s] storm: 1 effect + %d replays ✓", b.name, replay)
		})
	}
}

// Criterion 2: the cache is dropped mid-storm (Redis failover / FLUSHALL) ⇒
// still exactly 1 effect. Correctness comes from the DB unique constraint, not
// the cache.
func TestStormCacheKilledMidway(t *testing.T) {
	for _, b := range allBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			store, sink := b.fresh(t)
			swap := NewSwappableCache(NewMemCache())
			m := New(store, swap)
			key := "idem_killcache"
			hash := RequestHash("POST", "/kv", []byte(`{"k":"v"}`))

			var done int64
			var fresh, replay, other int64
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < stormN; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					out, err := m.Do(context.Background(), key, hash, sink.business)
					n := atomic.AddInt64(&done, 1)
					if n == stormN/2 {
						swap.Drop() // kill the cache halfway through the storm
					}
					switch {
					case err != nil:
						atomic.AddInt64(&other, 1)
						t.Errorf("unexpected error: %v", err)
					case out.Replayed:
						atomic.AddInt64(&replay, 1)
					default:
						atomic.AddInt64(&fresh, 1)
					}
				}()
			}
			close(start)
			wg.Wait()

			if !swap.Dropped() {
				t.Fatal("cache was never dropped")
			}
			if got := sink.count(t); got != 1 {
				t.Fatalf("[%s] effects=%d want 1 (cache killed)", b.name, got)
			}
			if fresh != 1 || replay != stormN-1 {
				t.Fatalf("[%s] fresh=%d replay=%d want 1/%d", b.name, fresh, replay, stormN-1)
			}
			t.Logf("[%s] cache-killed storm: 1 effect + %d replays ✓", b.name, replay)
		})
	}
}

// Criterion 3: same key + different body ⇒ 409 IDEMPOTENCY_KEY_REUSED on 100%
// of attempts.
func TestSameKeyDifferentBody409(t *testing.T) {
	for _, b := range allBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			store, sink := b.fresh(t)
			m := New(store, NewMemCache())
			key := "idem_reuse"
			hashA := RequestHash("POST", "/kv", []byte(`{"body":"A"}`))
			hashB := RequestHash("POST", "/kv", []byte(`{"body":"B"}`))

			// Establish the key with body A.
			if _, err := m.Do(context.Background(), key, hashA, sink.business); err != nil {
				t.Fatalf("establish: %v", err)
			}

			var conflicts int64
			var wg sync.WaitGroup
			for i := 0; i < stormN; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := m.Do(context.Background(), key, hashB, sink.business)
					var pe *shoperr.Error
					if asShopErr(err, &pe) && pe.Code == shoperr.CodeIdempotencyKeyReuse {
						atomic.AddInt64(&conflicts, 1)
					} else {
						t.Errorf("[%s] want IDEMPOTENCY_KEY_REUSED, got %v", b.name, err)
					}
				}()
			}
			wg.Wait()

			if conflicts != stormN {
				t.Fatalf("[%s] 409 reuse on %d/%d attempts, want 100%%", b.name, conflicts, stormN)
			}
			if got := sink.count(t); got != 1 {
				t.Fatalf("[%s] effects=%d want 1 (only body A ran)", b.name, got)
			}
			t.Logf("[%s] different-body: %d/%d ⇒ 409 IDEMPOTENCY_KEY_REUSED ✓", b.name, conflicts, stormN)
		})
	}
}

// Criterion 4: cold-cache replay p99 penalty < +20 ms. Measures replay latency
// with the cache warm (served from cache) vs cold (served from the DB re-read),
// and asserts the p99 delta is under the budget. The number is recorded in
// VERIFICATION.md.
func TestColdCacheReplayP99Penalty(t *testing.T) {
	const samples = 300
	for _, b := range allBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			store, sink := b.fresh(t)
			warmCache := NewMemCache()
			m := New(store, warmCache)
			key := "idem_p99"
			hash := RequestHash("POST", "/kv", []byte(`{"k":"v"}`))

			// Establish the key (one effect), warming the cache.
			if _, err := m.Do(context.Background(), key, hash, sink.business); err != nil {
				t.Fatalf("establish: %v", err)
			}

			warm := measure(t, samples, func() {
				out, err := m.Do(context.Background(), key, hash, sink.business)
				if err != nil || !out.Replayed {
					t.Fatalf("warm replay failed: %v replayed=%v", err, out.Replayed)
				}
			})

			// Cold: a Manager with NO cache — every replay hits the DB re-read path.
			cold := New(store, nil)
			coldLat := measure(t, samples, func() {
				out, err := cold.Do(context.Background(), key, hash, sink.business)
				if err != nil || !out.Replayed {
					t.Fatalf("cold replay failed: %v replayed=%v", err, out.Replayed)
				}
			})

			if got := sink.count(t); got != 1 {
				t.Fatalf("[%s] effects=%d want 1 (all replays)", b.name, got)
			}
			pWarm := p99(warm)
			pCold := p99(coldLat)
			penalty := pCold - pWarm
			t.Logf("[%s] replay p99: warm=%.3fms cold=%.3fms penalty=%.3fms (budget <20ms)",
				b.name, ms(pWarm), ms(pCold), ms(penalty))
			recordP99(b.name, ms(pWarm), ms(pCold), ms(penalty))
			if penalty >= 20*time.Millisecond {
				t.Fatalf("[%s] cold-cache p99 penalty %.3fms exceeds 20ms budget", b.name, ms(penalty))
			}
		})
	}
}

func measure(t *testing.T, n int, fn func()) []time.Duration {
	t.Helper()
	out := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		s := time.Now()
		fn()
		out[i] = time.Since(s)
	}
	return out
}

func p99(ds []time.Duration) time.Duration {
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)) * 0.99)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// p99 records for the VERIFICATION summary (printed at suite end).
var (
	p99mu   sync.Mutex
	p99recs []string
)

func recordP99(backend string, warm, cold, penalty float64) {
	p99mu.Lock()
	p99recs = append(p99recs, fmt.Sprintf("%s: warm p99=%.3fms cold p99=%.3fms penalty=%.3fms", backend, warm, cold, penalty))
	p99mu.Unlock()
}

func TestZZZReportP99(t *testing.T) {
	p99mu.Lock()
	defer p99mu.Unlock()
	if len(p99recs) == 0 {
		t.Skip("no p99 records (run TestColdCacheReplayP99Penalty first)")
	}
	for _, r := range p99recs {
		t.Logf("P99-PENALTY %s", r)
	}
}
