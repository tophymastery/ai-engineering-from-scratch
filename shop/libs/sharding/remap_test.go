package sharding

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSandbox(t *testing.T) *Cluster {
	t.Helper()
	cfg, err := LoadConfig("testdata/routing.4x256.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClusterFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// keysForShard brute-forces `count` distinct keys that hash to `shard`, so a
// test can deliberately drive the moving shard's dual-write path.
func keysForShard(shard, count int) []string {
	var out []string
	for i := 0; len(out) < count; i++ {
		k := "hot_" + strconv.Itoa(shard) + "_" + strconv.Itoa(i)
		if LogicalShard(k) == shard {
			out = append(out, k)
		}
	}
	return out
}

// TestSandboxRoutesEndToEnd is the DoD "sandbox routes end-to-end": keys stored
// across 4 fake physical targets, each landing on the target the router picks,
// and readable back through the cluster.
func TestSandboxRoutesEndToEnd(t *testing.T) {
	c := newSandbox(t)
	cfg, _ := LoadConfig("testdata/routing.4x256.json")
	tab, _ := cfg.Table()

	for i := 0; i < 5000; i++ {
		key := "cus_" + strconv.Itoa(i)
		val := "v" + strconv.Itoa(i)
		landed, err := c.Put(key, val)
		if err != nil {
			t.Fatal(err)
		}
		// It must land on exactly the router-chosen physical target.
		if want := tab[LogicalShard(key)]; landed != want {
			t.Fatalf("key %s landed on %s, router says %s", key, landed, want)
		}
		got, ok := c.Get(key)
		if !ok || got != val {
			t.Fatalf("readback %s = (%q,%v), want %q", key, got, ok, val)
		}
	}
	// All four targets should hold data.
	for _, name := range []string{"pg-0", "pg-1", "pg-2", "pg-3"} {
		if c.Store(name).Len() == 0 {
			t.Fatalf("target %s holds no rows — routing not spreading", name)
		}
	}
}

// TestRemapMovesShardWithData is the single-move correctness check: seed a
// shard, move it, confirm data moved, reads follow, and the old owner is clean.
func TestRemapMovesShardWithData(t *testing.T) {
	c := newSandbox(t)
	const shard = 100 // owned by pg-1 (64-127)
	hot := keysForShard(shard, 50)
	for i, k := range hot {
		if _, err := c.Put(k, "seed"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if c.Physical(shard) != "pg-1" {
		t.Fatalf("precondition: shard %d not on pg-1", shard)
	}
	rep, err := c.Move(shard, "pg-3", nil)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	t.Logf("%s", rep)
	if rep.From != "pg-1" || rep.To != "pg-3" || rep.Copied != len(hot) || rep.Verified != len(hot) {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if c.Physical(shard) != "pg-3" {
		t.Fatal("shard not cut over to pg-3")
	}
	if n := c.Store("pg-1").SnapshotShard(shard); len(n) != 0 {
		t.Fatalf("old owner pg-1 still holds %d rows for shard %d", len(n), shard)
	}
	for i, k := range hot {
		got, ok := c.Get(k)
		if !ok || got != "seed"+strconv.Itoa(i) {
			t.Fatalf("post-move read %s = (%q,%v)", k, got, ok)
		}
	}
}

// TestRemapUnderWriteLoad is the D6 test-criterion: "Sandbox remap under write
// load: zero misroutes, zero write errors." Concurrent writers hammer keys —
// including a hot set on the moving shard — while the coordinator repeatedly
// relocates that shard back and forth between two physical targets. Every write
// records its intended final value; after the storm every key is read back
// through the cluster and must equal that value (any miss = a misroute / lost
// write). Write errors are counted atomically.
func TestRemapUnderWriteLoad(t *testing.T) {
	c := newSandbox(t)
	const (
		shard      = 100
		numWriters = 8
		duration   = 2 * time.Second
	)
	hot := keysForShard(shard, 64)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var (
		wg          sync.WaitGroup
		writeErrs   atomic.Int64
		totalWrites atomic.Int64
		finals      = make([]map[string]string, numWriters)
		finalsMu    = make([]sync.Mutex, numWriters) // guards each writer's map at readout
	)

	// Each writer owns a disjoint key namespace (so each key has one writer and a
	// well-defined last value). Writer 0 additionally owns the hot shard-100 keys.
	for w := 0; w < numWriters; w++ {
		finals[w] = map[string]string{}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := finals[w]
			ver := 0
			// build this writer's key set: a spread set + (writer 0) the hot set
			spread := make([]string, 0, 256)
			for s := 0; s < 256; s++ {
				spread = append(spread, fmt.Sprintf("w%d_s%d", w, s))
			}
			var keys []string
			keys = append(keys, spread...)
			if w == 0 {
				keys = append(keys, hot...)
			}
			for {
				select {
				case <-ctx.Done():
					finalsMu[w].Lock()
					finalsMu[w].Unlock()
					return
				default:
				}
				for _, k := range keys {
					ver++
					val := "v" + strconv.Itoa(ver)
					if _, err := c.Put(k, val); err != nil {
						writeErrs.Add(1)
					} else {
						local[k] = val
						totalWrites.Add(1)
					}
				}
			}
		}(w)
	}

	// Coordinator: relocate the moving shard back and forth for the duration.
	var moves int
	var dualTotal int
	target := "pg-3"
	for {
		if ctx.Err() != nil {
			break
		}
		rep, err := c.Move(shard, target, nil)
		if err != nil {
			cancel()
			wg.Wait()
			t.Fatalf("move error during load (write/verify error): %v", err)
		}
		moves++
		dualTotal += rep.DualWrites
		if target == "pg-3" {
			target = "pg-1"
		} else {
			target = "pg-3"
		}
	}
	wg.Wait()

	// Verify: every recorded write is readable at its final value through the
	// cluster. A miss means a write was misrouted or lost during a move.
	misroutes := 0
	checked := 0
	for w := 0; w < numWriters; w++ {
		for k, want := range finals[w] {
			got, ok := c.Get(k)
			checked++
			if !ok || got != want {
				misroutes++
				if misroutes <= 5 {
					t.Errorf("misroute: key %s = (%q,%v), want %q", k, got, ok, want)
				}
			}
		}
	}

	t.Logf("moves=%d dual_writes=%d total_writes=%d keys_checked=%d write_errors=%d misroutes=%d final_owner(shard %d)=%s",
		moves, dualTotal, totalWrites.Load(), checked, writeErrs.Load(), misroutes, shard, c.Physical(shard))

	if writeErrs.Load() != 0 {
		t.Fatalf("write errors during remap = %d, want 0", writeErrs.Load())
	}
	if misroutes != 0 {
		t.Fatalf("misroutes during remap = %d, want 0", misroutes)
	}
	// Sanity: the storm must have actually exercised moves + the dual-write path.
	if moves < 5 {
		t.Fatalf("only %d moves — load window too small to be meaningful", moves)
	}
	if dualTotal == 0 {
		t.Fatal("no dual-writes observed — moving shard was never written during a window")
	}
}
