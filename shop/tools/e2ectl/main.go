// Command e2ectl is the single parser for the shared E2E env manifest (S-T8):
// deploy/e2e/topology.yaml plus an optional RUNTIME overlay
// (.run/e2e-overlay.yaml) that flips a slot's mode/real_cmd without editing the
// manifest. Every e2e-*.sh script shells out to it so the topology has exactly
// one source of truth and one resolver.
//
// Subcommands (all take: <topology.yaml> [overlay.yaml]):
//
//	plan    tab-separated resolved slots: name port mode contract real_cmd
//	routes  JSON route table for the gateway ([{prefix,upstream}], + fakes)
//	sync    like plan, but every slot whose real_cmd target EXISTS is forced to
//	        mode=real — the post-merge "merged implementations swap in" detector.
//	count   number of slots (services+bffs+fakes; excludes the gateway)
//
// Determinism: slots are emitted in manifest order; the gateway is never a slot.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

type slot struct {
	Name     string `yaml:"name"`
	Port     int    `yaml:"port"`
	Mode     string `yaml:"mode"`
	Contract string `yaml:"contract"`
	RealCmd  string `yaml:"real_cmd"`
}

type topology struct {
	Version  int    `yaml:"version"`
	Gateway  slot   `yaml:"gateway"`
	Services []slot `yaml:"services"`
}

type override struct {
	Mode    string `yaml:"mode"`
	RealCmd string `yaml:"real_cmd"`
}

type overlay struct {
	Overrides map[string]override `yaml:"overrides"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage:\n  e2ectl <plan|routes|sync|count> <topology.yaml> [overlay.yaml]\n  e2ectl set-overlay <overlay.yaml> <name> <mode> [real_cmd]")
		os.Exit(2)
	}
	cmd := os.Args[1]

	// set-overlay writes a single mode/real_cmd override into the runtime overlay,
	// preserving any existing overrides (used by tools/e2e-swap.sh). It is the only
	// subcommand that MUTATES a file; everything else is read-only resolution.
	if cmd == "set-overlay" {
		if len(os.Args) < 5 {
			fatal(fmt.Errorf("set-overlay needs <overlay.yaml> <name> <mode> [real_cmd]"))
		}
		if err := setOverlay(os.Args[2], os.Args[3], os.Args[4], strings.Join(os.Args[5:], " ")); err != nil {
			fatal(err)
		}
		return
	}

	topoPath := os.Args[2]
	overlayPath := ""
	if len(os.Args) > 3 {
		overlayPath = os.Args[3]
	}

	topo, err := loadTopology(topoPath)
	if err != nil {
		fatal(err)
	}
	ov, err := loadOverlay(overlayPath)
	if err != nil {
		fatal(err)
	}
	slots := resolve(topo, ov)

	switch cmd {
	case "plan":
		emitPlan(slots)
	case "sync":
		emitPlan(syncReal(slots))
	case "routes":
		emitRoutes(topo, slots)
	case "count":
		fmt.Println(len(slots))
	default:
		fatal(fmt.Errorf("unknown subcommand %q", cmd))
	}
}

// resolve applies overlay overrides (mode + optional real_cmd) onto the manifest.
func resolve(topo *topology, ov *overlay) []slot {
	out := make([]slot, 0, len(topo.Services))
	for _, s := range topo.Services {
		if o, ok := ov.Overrides[s.Name]; ok {
			if o.Mode != "" {
				s.Mode = o.Mode
			}
			if o.RealCmd != "" {
				s.RealCmd = o.RealCmd
			}
		}
		out = append(out, s)
	}
	return out
}

// syncReal is the post-merge detector: any slot with a real_cmd whose target
// binary EXISTS on disk is promoted to mode=real. This is what `make e2e-sync`
// uses so a merged slice implementation auto-swaps its stub. The first token of
// real_cmd is treated as the program (a path or a resolvable command name).
func syncReal(slots []slot) []slot {
	for i := range slots {
		s := &slots[i]
		if s.Mode == "real" || strings.TrimSpace(s.RealCmd) == "" {
			continue
		}
		if programExists(firstToken(s.RealCmd)) {
			s.Mode = "real"
		}
	}
	return slots
}

func programExists(prog string) bool {
	if prog == "" {
		return false
	}
	if strings.ContainsAny(prog, "/") {
		info, err := os.Stat(prog)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(prog)
	return err == nil
}

func firstToken(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func emitPlan(slots []slot) {
	// name \t port \t mode \t contract \t real_cmd
	for _, s := range slots {
		fmt.Printf("%s\t%d\t%s\t%s\t%s\n", s.Name, s.Port, s.Mode, s.Contract, s.RealCmd)
	}
}

type route struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
}

func emitRoutes(topo *topology, slots []slot) {
	routes := make([]route, 0, len(slots))
	for _, s := range slots {
		routes = append(routes, route{
			Prefix:   "/" + s.Name + "/",
			Upstream: fmt.Sprintf("http://localhost:%d", s.Port),
		})
	}
	b, _ := json.MarshalIndent(routes, "", "  ")
	fmt.Println(string(b))
}

// setOverlay upserts one override into the overlay file (creating it if absent).
func setOverlay(path, name, mode, realCmd string) error {
	o, err := loadOverlay(path)
	if err != nil {
		return err
	}
	ov := o.Overrides[name]
	ov.Mode = mode
	if realCmd != "" {
		ov.RealCmd = realCmd
	}
	o.Overrides[name] = ov
	b, err := yaml.Marshal(o)
	if err != nil {
		return err
	}
	header := "# .run/e2e-overlay.yaml — RUNTIME stub->real swaps for the shared E2E env\n" +
		"# (S-T8). Written by tools/e2e-swap.sh / `make e2e-sync`; NEVER hand-edit the\n" +
		"# manifest for a swap. tools/e2e-up.sh merges this over deploy/e2e/topology.yaml\n" +
		"# on every invocation.\n"
	return os.WriteFile(path, append([]byte(header), b...), 0o644)
}

func loadTopology(path string) (*topology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t topology
	if err := yaml.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(t.Services) == 0 {
		return nil, fmt.Errorf("%s: no services", path)
	}
	return &t, nil
}

func loadOverlay(path string) (*overlay, error) {
	if path == "" {
		return &overlay{Overrides: map[string]override{}}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &overlay{Overrides: map[string]override{}}, nil
		}
		return nil, err
	}
	var o overlay
	if err := yaml.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if o.Overrides == nil {
		o.Overrides = map[string]override{}
	}
	return &o, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "e2ectl:", err)
	os.Exit(1)
}
