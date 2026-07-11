package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// loadJSON reads a JSON file into a generic map.
func loadJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", path, err)
	}
	return m, nil
}

// cmdDiff enforces the D30 additive-only rule between two schema versions of the
// same topic. It walks both trees and collects every breaking change; a non-empty
// list is a hard failure (exit 1). A purely additive change (new optional fields)
// yields no breaks and exits 0.
func cmdDiff(oldPath, newPath string) error {
	oldS, err := loadJSON(oldPath)
	if err != nil {
		return err
	}
	newS, err := loadJSON(newPath)
	if err != nil {
		return err
	}
	breaks := diffSchema("", oldS, newS)
	if len(breaks) == 0 {
		fmt.Printf("diff: %s -> %s is ADDITIVE-ONLY (D30 compatible)\n", oldPath, newPath)
		return nil
	}
	sort.Strings(breaks)
	var b strings.Builder
	fmt.Fprintf(&b, "D30 violation: %s -> %s is NOT additive (%d breaking change(s)):\n", oldPath, newPath, len(breaks))
	for _, m := range breaks {
		fmt.Fprintf(&b, "  - %s\n", m)
	}
	b.WriteString("  fix: shape changes require a new .v2 topic + dual-publish window (see order.paid.v2)")
	return fmt.Errorf("%s", b.String())
}

// diffSchema recursively compares two JSON-Schema object nodes and returns a list
// of D30-breaking changes: property removal/rename, type change, required-field
// addition, and enum-value removal. Additions of new optional properties are OK.
func diffSchema(path string, oldN, newN map[string]any) []string {
	var out []string
	at := func(f string) string {
		if path == "" {
			return f
		}
		return path + "." + f
	}

	// type change (includes const, which is a singleton type).
	if ot, nt := typeOf(oldN), typeOf(newN); ot != "" && nt != "" && ot != nt {
		out = append(out, fmt.Sprintf("%s: type changed %q -> %q", nonEmpty(path, "<root>"), ot, nt))
	}

	// required additions (a field that was optional/absent becoming required
	// breaks producers/consumers that never populated it).
	oldReq, newReq := stringSet(oldN["required"]), stringSet(newN["required"])
	for f := range newReq {
		if !oldReq[f] {
			out = append(out, fmt.Sprintf("%s: newly REQUIRED (required-addition)", at(f)))
		}
	}

	// enum narrowing (removing an accepted value breaks existing producers).
	oldEnum, newEnum := valueSet(oldN["enum"]), valueSet(newN["enum"])
	if len(oldEnum) > 0 && len(newEnum) > 0 {
		for v := range oldEnum {
			if !newEnum[v] {
				out = append(out, fmt.Sprintf("%s: enum value %q removed", nonEmpty(path, "<root>"), v))
			}
		}
	}

	// properties: removals are breaking; survivors recurse; new ones are additive.
	oldProps := asMap(oldN["properties"])
	newProps := asMap(newN["properties"])
	if oldProps != nil {
		for name, ov := range oldProps {
			nv, ok := newProps[name]
			if !ok {
				out = append(out, fmt.Sprintf("%s: property REMOVED or renamed", at(name)))
				continue
			}
			if om, nm := asMap(ov), asMap(nv); om != nil && nm != nil {
				out = append(out, diffSchema(at(name), om, nm)...)
			}
		}
	}

	// array items recurse too.
	if oi, ni := asMap(oldN["items"]), asMap(newN["items"]); oi != nil && ni != nil {
		out = append(out, diffSchema(at("[]"), oi, ni)...)
	}
	return out
}

// typeOf returns a comparable type token: the JSON-Schema "type" (normalised) or
// a "const:<v>" token when the node pins a constant.
func typeOf(n map[string]any) string {
	if c, ok := n["const"]; ok {
		return fmt.Sprintf("const:%v", c)
	}
	switch t := n["type"].(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, fmt.Sprintf("%v", x))
		}
		sort.Strings(parts)
		return strings.Join(parts, "|")
	}
	return ""
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func valueSet(v any) map[string]bool {
	out := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			out[fmt.Sprintf("%v", x)] = true
		}
	}
	return out
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
