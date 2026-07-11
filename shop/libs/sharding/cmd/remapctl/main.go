// Command remapctl is the online shard-remap tool (D6 scope item 3): it moves a
// logical shard from one physical target to another with the
// copy → dual-write → verify → cutover sequence, optionally under concurrent
// synthetic write load, and prints the move report (copied / verified / cleaned
// / write_errors / misroutes).
//
// In S-T4 it drives the in-memory SANDBOX (4 fake physical targets) so the
// sequence is demonstrable and testable end-to-end with zero infrastructure.
// Migrating real service tables (real PG) is V-T26/V-T27; this tool is the
// runbook-shaped harness those slices adopt.
//
// Usage:
//
//	go run ./cmd/remapctl -config testdata/routing.4x256.json -shard 100 -to pg-3
//	go run ./cmd/remapctl -config testdata/routing.4x256.json -shard 100 -to pg-3 \
//	    -load -writers 8 -duration 2s -seed 2000
//
// Exit code is non-zero on any move error, write error, or misroute — so it
// doubles as a CI-runnable smoke check of the remap machinery.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shop-platform/shop/libs/sharding"
)

func main() {
	cfgPath := flag.String("config", "", "path to the routing map (JSON or restricted YAML)")
	shard := flag.Int("shard", -1, "logical shard to move (0..255)")
	to := flag.String("to", "", "destination physical target name")
	load := flag.Bool("load", false, "apply concurrent write load during the move")
	writers := flag.Int("writers", 8, "number of concurrent writers when -load")
	dur := flag.Duration("duration", 2*time.Second, "load duration when -load")
	seed := flag.Int("seed", 2000, "number of keys to pre-seed across shards")
	flag.Parse()

	if *cfgPath == "" || *shard < 0 || *to == "" {
		fmt.Fprintln(os.Stderr, "remapctl: -config, -shard and -to are required")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := sharding.LoadConfig(*cfgPath)
	if err != nil {
		fatal(err)
	}
	cl, err := sharding.NewClusterFromConfig(cfg)
	if err != nil {
		fatal(err)
	}

	// Pre-seed keys across all shards so the move has data to copy.
	seeded := map[string]string{}
	for i := 0; i < *seed; i++ {
		k := "seed_" + strconv.Itoa(i)
		v := "s" + strconv.Itoa(i)
		if _, err := cl.Put(k, v); err != nil {
			fatal(err)
		}
		seeded[k] = v
	}
	fmt.Printf("seeded %d keys; moving logical shard %d from %s to %s\n",
		len(seeded), *shard, cl.Physical(*shard), *to)

	var (
		writeErrs atomic.Int64
		totWrites atomic.Int64
		wg        sync.WaitGroup
		finals    = make([]map[string]string, 0, *writers)
		fmu       sync.Mutex
	)

	ctx, cancel := context.WithCancel(context.Background())
	if *load {
		for w := 0; w < *writers; w++ {
			local := map[string]string{}
			fmu.Lock()
			finals = append(finals, local)
			fmu.Unlock()
			wg.Add(1)
			go func(w int, local map[string]string) {
				defer wg.Done()
				ver := 0
				for {
					select {
					case <-ctx.Done():
						return
					default:
					}
					for s := 0; s < sharding.NumLogicalShards; s++ {
						ver++
						k := fmt.Sprintf("w%d_s%d", w, s)
						v := "v" + strconv.Itoa(ver)
						if _, err := cl.Put(k, v); err != nil {
							writeErrs.Add(1)
						} else {
							local[k] = v
							totWrites.Add(1)
						}
					}
				}
			}(w, local)
		}
	}

	// Perform the move (optionally repeat back-and-forth for the load duration).
	start := time.Now()
	rep, err := cl.Move(*shard, *to, &sharding.RemapHooks{
		OnDualWriteStart: func() { fmt.Println("  phase: dual-write window OPEN") },
		AfterBackfill:    func(n int) { fmt.Printf("  phase: backfill copied %d rows\n", n) },
		BeforeCutover:    func() { fmt.Println("  phase: verify + cutover (shard frozen)") },
		AfterCutover:     func() { fmt.Println("  phase: cutover DONE") },
	})
	if err != nil {
		cancel()
		wg.Wait()
		fatal(fmt.Errorf("move failed: %w", err))
	}
	if *load {
		// keep hammering for the remainder of the window to exercise the settled
		// routing, then stop.
		for time.Since(start) < *dur {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		wg.Wait()
	}

	// Verify every recorded write is readable at its final value.
	misroutes := 0
	fmu.Lock()
	for _, local := range finals {
		for k, want := range local {
			if got, ok := cl.Get(k); !ok || got != want {
				misroutes++
			}
		}
	}
	fmu.Unlock()
	for k, want := range seeded {
		if got, ok := cl.Get(k); !ok || got != want {
			// seeded keys on the moved shard are expected to survive the move.
			misroutes++
		}
	}

	rep.WriteErrors = int(writeErrs.Load())
	rep.Misroutes = misroutes
	fmt.Println(rep.String())
	fmt.Printf("load: writers=%d total_writes=%d elapsed=%s\n", *writers, totWrites.Load(), time.Since(start))

	if rep.WriteErrors != 0 || rep.Misroutes != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: write_errors=%d misroutes=%d\n", rep.WriteErrors, rep.Misroutes)
		os.Exit(1)
	}
	fmt.Println("OK: zero write errors, zero misroutes")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "remapctl:", err)
	os.Exit(1)
}
