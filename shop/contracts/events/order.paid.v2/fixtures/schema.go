// Package fixtures is the D30 worked example: it proves a producer can dual-publish
// order.paid (v1) and order.paid.v2 during the deprecation window, with each
// consumer generation reading only its own topic — both green.
//
// It ships a tiny stdlib-only JSON-Schema validator (draft-07 subset: type,
// required, additionalProperties:false, const, properties, nested objects) so the
// dual-publish test validates the emitted envelopes against the REAL registry
// schema files (../v2.schema.json and ../../order.paid/v1.schema.json). That ties
// the example to the same contracts registryctl gates — not a parallel copy.
package fixtures

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// LoadSchema reads a JSON-Schema file into a generic map.
func LoadSchema(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Validate checks value against schema and returns all violations (empty = valid).
func Validate(schema map[string]any, value any) []string {
	return validateNode("$", schema, value)
}

func validateNode(path string, schema map[string]any, value any) []string {
	var out []string

	if c, ok := schema["const"]; ok {
		if fmt.Sprintf("%v", c) != fmt.Sprintf("%v", value) {
			out = append(out, fmt.Sprintf("%s: want const %v, got %v", path, c, value))
		}
		return out
	}

	switch typeOf(schema) {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object, got %T", path, value)}
		}
		props, _ := schema["properties"].(map[string]any)
		for _, r := range asStrings(schema["required"]) {
			if _, ok := obj[r]; !ok {
				out = append(out, fmt.Sprintf("%s.%s: required field missing", path, r))
			}
		}
		if add, ok := schema["additionalProperties"].(bool); ok && !add {
			for k := range obj {
				if _, declared := props[k]; !declared {
					out = append(out, fmt.Sprintf("%s.%s: additional property not allowed", path, k))
				}
			}
		}
		for name, sub := range props {
			if sm, ok := sub.(map[string]any); ok {
				if v, present := obj[name]; present {
					out = append(out, validateNode(path+"."+name, sm, v)...)
				}
			}
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want array, got %T", path, value)}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, v := range arr {
				out = append(out, validateNode(fmt.Sprintf("%s[%d]", path, i), items, v)...)
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			out = append(out, fmt.Sprintf("%s: want string, got %T", path, value))
		}
	case "integer":
		if !isNumber(value) {
			out = append(out, fmt.Sprintf("%s: want integer, got %T", path, value))
		}
	case "number":
		if !isNumber(value) {
			out = append(out, fmt.Sprintf("%s: want number, got %T", path, value))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			out = append(out, fmt.Sprintf("%s: want boolean, got %T", path, value))
		}
	}
	sort.Strings(out)
	return out
}

func typeOf(schema map[string]any) string {
	if s, ok := schema["type"].(string); ok {
		return s
	}
	return ""
}

func asStrings(v any) []string {
	var out []string
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, float32, int, int64, json.Number:
		return true
	}
	return false
}

// Pretty renders a violation list for test failure messages.
func Pretty(v []string) string { return strings.Join(v, "; ") }
