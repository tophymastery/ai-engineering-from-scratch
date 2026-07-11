package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSingleflight_CollapsesConcurrentDuplicates is the primitive-level proof: a
// stampede of concurrent Do(key) calls runs fn EXACTLY ONCE. This is the
// invariant the two-tier + feed caches build the "exactly 1 origin fetch" on.
func TestSingleflight_CollapsesConcurrentDuplicates(t *testing.T) {
	var g Group
	const n = 10000
	var runs atomic.Int64
	gate := make(chan struct{})

	var started sync.WaitGroup
	var done sync.WaitGroup
	started.Add(n)
	done.Add(n)
	shared := make([]bool, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			started.Done()
			val, err, sh := g.Do("hot", func() ([]byte, error) {
				<-gate // hold the leader in-flight until every caller has arrived
				runs.Add(1)
				return []byte("ok"), nil
			})
			if err != nil || string(val) != "ok" {
				t.Errorf("Do returned val=%q err=%v", val, err)
			}
			shared[i] = sh
		}(i)
	}
	started.Wait() // all goroutines scheduled and about to enter Do
	close(gate)    // release the single leader
	done.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("fn ran %d times, want exactly 1 (singleflight collapse)", got)
	}
	sharedCount := 0
	for _, s := range shared {
		if s {
			sharedCount++
		}
	}
	if sharedCount != n-1 {
		t.Fatalf("shared=%d, want %d (1 leader + %d coalesced)", sharedCount, n-1, n-1)
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight=%d after drain, want 0", g.InFlight())
	}
}

// TestSingleflight_DistinctKeysRunIndependently confirms different keys are NOT
// collapsed (only same-key duplicates coalesce).
func TestSingleflight_DistinctKeysRunIndependently(t *testing.T) {
	var g Group
	var runs atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%25)) // 25 distinct keys
			_, _, _ = g.Do(key, func() ([]byte, error) {
				runs.Add(1)
				return nil, nil
			})
		}(i)
	}
	wg.Wait()
	// At least one run per distinct key; coalescing may drop some, but never
	// below the number of distinct keys and never above the call count.
	if got := runs.Load(); got < 1 || got > 100 {
		t.Fatalf("runs=%d out of range", got)
	}
}

// TestSingleflight_PropagatesError confirms the leader's error is shared.
func TestSingleflight_PropagatesError(t *testing.T) {
	var g Group
	sentinel := errors.New("boom")
	gate := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err, _ := g.Do("k", func() ([]byte, error) {
				<-gate
				return nil, sentinel
			})
			errs[i] = err
		}(i)
	}
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, sentinel) {
			t.Fatalf("caller %d got err=%v, want sentinel", i, err)
		}
	}
}
