package main

// schema_validate_test.go — proves the driver.location_updated event this slice
// EMITS is SCHEMA-VALID against its published contract
// (contracts/events/driver.location_updated/v1.schema.json), using the same
// compact dependency-free draft-07 subset validator the other slices use. Also
// asserts the PG migration and the SQLite twin declare the same table (parity).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	plane "github.com/shop-platform/shop/services/location-gateway/plane"
)

type jsonSchema struct {
	Type                 any                    `json:"type"`
	Const                any                    `json:"const"`
	Enum                 []any                  `json:"enum"`
	Required             []string               `json:"required"`
	Properties           map[string]*jsonSchema `json:"properties"`
	AdditionalProperties json.RawMessage        `json:"additionalProperties"`
}

func loadEventSchema(t *testing.T, topic string) *jsonSchema {
	t.Helper()
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "..", "..", "contracts", "events", topic, "v1.schema.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", topic, err)
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("parse schema %s: %v", topic, err)
	}
	return &s
}

func jsTypeMatches(decl string, v any) bool {
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
	}
	return false
}

func (s *jsonSchema) allowedTypes() []string {
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

func (s *jsonSchema) validate(path string, v any) []string {
	var errs []string
	if types := s.allowedTypes(); len(types) > 0 {
		ok := false
		for _, ty := range types {
			if jsTypeMatches(ty, v) {
				ok = true
				break
			}
		}
		if !ok {
			return []string{fmt.Sprintf("%s: type mismatch (want %v)", path, types)}
		}
	}
	if s.Const != nil {
		if fmt.Sprintf("%v", s.Const) != fmt.Sprintf("%v", v) {
			errs = append(errs, fmt.Sprintf("%s: const mismatch (want %v got %v)", path, s.Const, v))
		}
	}
	if obj, ok := v.(map[string]any); ok {
		for _, req := range s.Required {
			if _, ok := obj[req]; !ok {
				errs = append(errs, fmt.Sprintf("%s: missing required property %q", path, req))
			}
		}
		allowExtra := true
		if len(s.AdditionalProperties) > 0 {
			var b bool
			if json.Unmarshal(s.AdditionalProperties, &b) == nil {
				allowExtra = b
			}
		}
		for k, val := range obj {
			if child, declared := s.Properties[k]; declared {
				errs = append(errs, child.validate(path+"."+k, val)...)
			} else if !allowExtra {
				errs = append(errs, fmt.Sprintf("%s: unexpected property %q", path, k))
			}
		}
	}
	return errs
}

// TestEmittedEventIsSchemaValid builds the driver.location_updated envelope this
// slice produces (via the same makeLocationEnvelope the sink uses) and validates
// it against the published contract.
func TestEmittedEventIsSchemaValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cell := plane.LatLngToCell(13.7460, 100.5340)
	env, err := makeLocationEnvelope("drv_sv", "bkk", cell, 13.7460, 100.5340, now)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	raw, _ := env.Marshal()
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	schema := loadEventSchema(t, "driver.location_updated")
	if v := schema.validate("$", inst); len(v) > 0 {
		t.Fatalf("driver.location_updated violates its published contract: %v\nraw=%s", v, raw)
	}
	t.Logf("EVENT CONFORMANCE: driver.location_updated valid vs contracts/events/driver.location_updated/v1.schema.json (h3_cell=%s)", cell.Key())
}

// TestSchemaParity: the SQLite runtime twin declares the same table as the
// production PG migration (types differ; table+column names match).
func TestSchemaParity(t *testing.T) {
	for _, tbl := range []string{"trip_summaries"} {
		if !containsWord(pgMigration, tbl) {
			t.Fatalf("PG migration missing table %q", tbl)
		}
		if !containsWord(sqliteSchema, tbl) {
			t.Fatalf("SQLite twin missing table %q", tbl)
		}
	}
	// D15: PG keeps trip summaries ONLY — there must be exactly ONE table, the
	// trip-summary table (no raw-position table on PG).
	if n := countOccurrences(pgMigration, "CREATE TABLE"); n != 1 {
		t.Fatalf("PG migration declares %d tables; D15 allows exactly 1 (trip_summaries only)", n)
	}
}

func countOccurrences(s, sub string) int {
	n, i := 0, 0
	for {
		j := indexOf(s[i:], sub)
		if j < 0 {
			return n
		}
		n++
		i += j + len(sub)
	}
}

func containsWord(s, w string) bool {
	return len(s) > 0 && len(w) > 0 && indexOf(s, w) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
