package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// This file is a compact, dependency-free JSON-Schema (draft-07 subset)
// validator used ONLY by the log-schema test. It supports exactly the features
// contracts/log-schema.json uses: type (incl. union types), required, enum,
// properties, additionalProperties (bool or schema). Keeping it in-package and
// stdlib-only means libs/logging ships zero external dependencies.

type schema struct {
	Type                 any                `json:"type"`
	Enum                 []any              `json:"enum"`
	Required             []string           `json:"required"`
	Properties           map[string]*schema `json:"properties"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
	// unused draft-07 fields are ignored
}

func loadSchema() (*schema, error) {
	// contracts/log-schema.json lives at repo-root/contracts; this test runs in
	// libs/logging, so climb to the shop root.
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "..", "..", "contracts", "log-schema.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func typeMatches(decl string, v any) bool {
	switch decl {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "number":
		_, ok := v.(float64)
		return ok
	case "null":
		return v == nil
	}
	return false
}

func (s *schema) allowedTypes() []string {
	switch t := s.Type.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// validate returns a list of violation strings ("" slice means valid).
func (s *schema) validate(path string, v any) []string {
	var errs []string
	if types := s.allowedTypes(); len(types) > 0 {
		ok := false
		for _, t := range types {
			if typeMatches(t, v) {
				ok = true
				break
			}
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: type mismatch (want %v)", path, types))
			return errs // no point recursing on a type mismatch
		}
	}
	if len(s.Enum) > 0 {
		found := false
		for _, e := range s.Enum {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: value %v not in enum", path, v))
		}
	}
	obj, isObj := v.(map[string]any)
	if isObj {
		for _, req := range s.Required {
			if _, ok := obj[req]; !ok {
				errs = append(errs, fmt.Sprintf("%s: missing required property %q", path, req))
			}
		}
		// additionalProperties handling.
		allowExtra := true
		var extraSchema *schema
		if len(s.AdditionalProperties) > 0 {
			var b bool
			if json.Unmarshal(s.AdditionalProperties, &b) == nil {
				allowExtra = b
			} else {
				var sub schema
				if json.Unmarshal(s.AdditionalProperties, &sub) == nil {
					extraSchema = &sub
				}
			}
		}
		for k, val := range obj {
			child, declared := s.Properties[k]
			if declared {
				errs = append(errs, child.validate(path+"."+k, val)...)
				continue
			}
			if extraSchema != nil {
				errs = append(errs, extraSchema.validate(path+"."+k, val)...)
			} else if !allowExtra {
				errs = append(errs, fmt.Sprintf("%s: unexpected property %q", path, k))
			}
		}
	}
	return errs
}

func validateLine(s *schema, line []byte) []string {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		return []string{"invalid JSON: " + err.Error()}
	}
	return s.validate("$", v)
}

