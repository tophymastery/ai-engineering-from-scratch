package sharding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the on-disk routing map: which physical target owns which logical
// shards. It is the single source of truth the Router loads and hot-reloads.
//
// Canonical format is JSON (pure stdlib). A restricted YAML dialect is also
// accepted for operator ergonomics (see parseYAML). Both express the same
// shape:
//
//	{
//	  "version": 1,
//	  "targets": { "pg-0": "host=pg0 dbname=cell", "pg-1": "host=pg1 dbname=cell" },
//	  "assignments": [
//	    { "target": "pg-0", "shards": "0-127" },
//	    { "target": "pg-1", "shards": "128-255" }
//	  ]
//	}
//
// A "shards" spec is a comma-separated list of single shards ("42") and
// inclusive ranges ("0-63"). Every logical shard [0,NumLogicalShards) must be
// assigned exactly once and every referenced target must be declared in targets
// — Validate enforces both, so a typo fails the load instead of silently
// black-holing a shard.
type Config struct {
	Version     int               `json:"version"`
	Targets     map[string]string `json:"targets"`
	Assignments []Assignment      `json:"assignments"`
}

// Assignment binds a contiguous or listed set of logical shards to one target.
type Assignment struct {
	Target string `json:"target"`
	Shards string `json:"shards"`
}

// LoadConfig reads a routing map from path, dispatching on extension:
// .json → JSON, .yaml/.yml → the restricted YAML dialect. The returned Config
// is validated (full coverage, known targets).
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := parseYAML(raw, &cfg); err != nil {
			return nil, fmt.Errorf("sharding: parse yaml %s: %w", path, err)
		}
	default:
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("sharding: parse json %s: %w", path, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Table expands the Config into a dense [NumLogicalShards] target lookup. Caller
// must have Validate()d (LoadConfig does).
func (c *Config) Table() ([NumLogicalShards]string, error) {
	var t [NumLogicalShards]string
	assigned := make([]bool, NumLogicalShards)
	for _, a := range c.Assignments {
		shards, err := parseShardSpec(a.Shards)
		if err != nil {
			return t, err
		}
		for _, s := range shards {
			t[s] = a.Target
			assigned[s] = true
		}
	}
	for s, ok := range assigned {
		if !ok {
			return t, fmt.Errorf("sharding: logical shard %d unassigned", s)
		}
	}
	return t, nil
}

// Validate checks target references and full, non-overlapping shard coverage.
func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("sharding: config has no targets")
	}
	seen := make([]int, NumLogicalShards)
	for i := range seen {
		seen[i] = -1
	}
	for ai, a := range c.Assignments {
		if _, ok := c.Targets[a.Target]; !ok {
			return fmt.Errorf("sharding: assignment %d references unknown target %q", ai, a.Target)
		}
		shards, err := parseShardSpec(a.Shards)
		if err != nil {
			return fmt.Errorf("sharding: assignment %d (%s): %w", ai, a.Target, err)
		}
		for _, s := range shards {
			if seen[s] >= 0 {
				return fmt.Errorf("sharding: logical shard %d assigned to both %q and %q",
					s, c.Assignments[seen[s]].Target, a.Target)
			}
			seen[s] = ai
		}
	}
	for s, ai := range seen {
		if ai < 0 {
			return fmt.Errorf("sharding: logical shard %d unassigned (config must cover 0..%d)", s, NumLogicalShards-1)
		}
	}
	return nil
}

// parseShardSpec expands "0-63,200,250-255" into explicit shard numbers.
func parseShardSpec(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty shard spec")
	}
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad range %q", part)
			}
			if lo > hi || lo < 0 || hi >= NumLogicalShards {
				return nil, fmt.Errorf("range %q out of [0,%d)", part, NumLogicalShards)
			}
			for s := lo; s <= hi; s++ {
				out = append(out, s)
			}
		} else {
			s, err := strconv.Atoi(part)
			if err != nil || s < 0 || s >= NumLogicalShards {
				return nil, fmt.Errorf("bad shard %q", part)
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// parseYAML reads the restricted YAML dialect used for routing maps into cfg.
// It is intentionally NOT a general YAML parser (no external deps, D6 wants
// dependency-light): it understands exactly the three top-level keys
// version/targets/assignments with 2-space indentation, which is all a routing
// map needs. Anything outside that shape is a parse error.
func parseYAML(raw []byte, cfg *Config) error {
	cfg.Targets = map[string]string{}
	lines := strings.Split(string(raw), "\n")
	section := ""
	var cur *Assignment
	flush := func() {
		if cur != nil {
			cfg.Assignments = append(cfg.Assignments, *cur)
			cur = nil
		}
	}
	for n, ln := range lines {
		// strip comments and trailing space; ignore blank lines.
		if i := strings.IndexByte(ln, '#'); i >= 0 {
			ln = ln[:i]
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		trim := strings.TrimSpace(ln)

		switch {
		case indent == 0:
			flush()
			key, val := splitKV(trim)
			switch key {
			case "version":
				v, err := strconv.Atoi(strings.TrimSpace(val))
				if err != nil {
					return fmt.Errorf("line %d: bad version %q", n+1, val)
				}
				cfg.Version = v
				section = ""
			case "targets":
				section = "targets"
			case "assignments":
				section = "assignments"
			default:
				return fmt.Errorf("line %d: unknown top-level key %q", n+1, key)
			}
		case section == "targets":
			key, val := splitKV(trim)
			cfg.Targets[key] = unquote(strings.TrimSpace(val))
		case section == "assignments":
			if strings.HasPrefix(trim, "- ") {
				flush()
				cur = &Assignment{}
				trim = strings.TrimSpace(trim[2:])
			}
			if cur == nil {
				return fmt.Errorf("line %d: assignment field before '-' item", n+1)
			}
			key, val := splitKV(trim)
			switch key {
			case "target":
				cur.Target = unquote(strings.TrimSpace(val))
			case "shards":
				cur.Shards = unquote(strings.TrimSpace(val))
			default:
				return fmt.Errorf("line %d: unknown assignment field %q", n+1, key)
			}
		default:
			return fmt.Errorf("line %d: unexpected indented line outside a section", n+1)
		}
	}
	flush()
	return nil
}

func splitKV(s string) (string, string) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:i]), s[i+1:]
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
