// Command seedctl reads a declarative YAML scenario (03 §3) and populates a
// target stack THROUGH PUBLIC APIs with deterministic, factory-built data. Same
// seed + scenario ⇒ byte-identical dataset on rerun (the canonical JSON dump is
// what CI hash-compares).
//
//	seedctl -scenario scenarios/lunch-rush.yaml -target http://localhost:8081
//	seedctl -scenario scenarios/demo-small.yaml -dump-only -out dump.json
//
// Flags:
//
//	-scenario  path to a scenarios/*.yaml file (required)
//	-target    base URL of the stack to seed via public APIs (empty => dump only)
//	-out       write the canonical JSON dump here (default: stdout)
//	-dump-only build + dump without contacting any target (determinism checks)
//	-quiet     suppress the human summary on stderr
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	scenario := flag.String("scenario", "", "path to a scenarios/*.yaml file (required)")
	target := flag.String("target", "", "base URL to seed via public APIs (empty => dump only)")
	out := flag.String("out", "", "write canonical JSON dump here (default: stdout)")
	dumpOnly := flag.Bool("dump-only", false, "build + dump without contacting any target")
	quiet := flag.Bool("quiet", false, "suppress the human summary on stderr")
	flag.Parse()

	if *scenario == "" {
		fmt.Fprintln(os.Stderr, "seedctl: -scenario is required")
		flag.Usage()
		os.Exit(2)
	}

	name := strings.TrimSuffix(filepath.Base(*scenario), filepath.Ext(*scenario))
	s, err := LoadScenario(*scenario, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seedctl: %v\n", err)
		os.Exit(1)
	}

	ds := Build(s)
	dump := ds.Canonical()

	// Seed the target through public APIs (unless dump-only / no target).
	var sink Sink = NullSink{}
	if !*dumpOnly && *target != "" {
		sink = NewKVSink(*target)
	}
	written, err := push(sink, ds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seedctl: seeding %s failed: %v\n", sink.Name(), err)
		os.Exit(1)
	}

	// Write the canonical dump (stdout by default) — this is the hashable artefact.
	if *out != "" {
		if err := os.WriteFile(*out, dump, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "seedctl: write %s: %v\n", *out, err)
			os.Exit(1)
		}
	} else {
		os.Stdout.Write(dump)
	}

	if !*quiet {
		sum := sha256.Sum256(dump)
		fmt.Fprintf(os.Stderr,
			"seedctl: scenario=%s seed=%d region=%s sink=%s\n"+
				"  counts: users=%d merchants=%d menu_items=%d carts=%d drivers=%d orders=%d (written=%d)\n"+
				"  canonical-dump sha256=%x\n",
			s.Name, s.Seed, s.Region, sink.Name(),
			ds.Counts["users"], ds.Counts["merchants"], ds.Counts["menu_items"],
			ds.Counts["carts"], ds.Counts["drivers"], ds.Counts["orders"], written,
			sum)
	}
}
