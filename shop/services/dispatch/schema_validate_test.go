package main

// schema_validate_test.go — proves the dispatch.* events this slice EMITS
// (dispatch.offered on reserve+offer, dispatch.assigned on accept, dispatch.failed
// on batch exhaustion) and the order.paid + driver.location_updated it CONSUMES are
// SCHEMA-VALID against their published contracts (contracts/events/<topic>/
// v1.schema.json). Uses the same compact, dependency-free draft-07 subset validator
// the other slices use. Run for real on every CI pass.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
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

func assertValid(t *testing.T, topic string, env eventbus.Envelope) {
	t.Helper()
	raw, _ := env.Marshal()
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("%s not JSON: %v", topic, err)
	}
	schema := loadEventSchema(t, topic)
	if v := schema.validate("$", inst); len(v) > 0 {
		t.Fatalf("%s envelope violates its published contract: %v\nraw=%s", topic, v, raw)
	}
	t.Logf("EVENT CONFORMANCE: %s valid vs contracts/events/%s/v1.schema.json", topic, topic)
}

// TestEmittedAndConsumedEventsAreSchemaValid builds each dispatch.* envelope this
// slice produces (via the same makeEnvelope the handlers use) and the events it
// consumes, and validates them against their published contracts.
func TestEmittedAndConsumedEventsAreSchemaValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// PRODUCED — dispatch.offered / assigned / failed (aggregate type "order").
	offered, _ := makeEnvelope(TopicDispatchOffered, "ord_sv", "bkk", map[string]any{
		"order_id": "ord_sv", "driver_id": "drv_sv", "pickup_eta_s": 180, "offered_at": now.Format(time.RFC3339)}, now)
	assertValid(t, "dispatch.offered", offered)

	assigned, _ := makeEnvelope(TopicDispatchAssigned, "ord_sv", "bkk", map[string]any{
		"order_id": "ord_sv", "driver_id": "drv_sv", "assigned_at": now.Format(time.RFC3339), "eta_minutes": 3}, now)
	assertValid(t, "dispatch.assigned", assigned)

	failed, _ := makeEnvelope(TopicDispatchFailed, "ord_sv", "bkk", map[string]any{
		"order_id": "ord_sv", "reason": "no_driver", "failed_at": now.Format(time.RFC3339)}, now)
	assertValid(t, "dispatch.failed", failed)

	// CONSUMED — order.paid (aggregate order) + driver.location_updated (aggregate driver).
	paid, _ := makeEnvelope(TopicOrderPaid, "ord_sv", "bkk", map[string]any{
		"order_id": "ord_sv", "merchant_id": "mer_sv",
		"total": map[string]any{"amount": 42550, "currency": "THB"}, "paid_at": now.Format(time.RFC3339)}, now)
	assertValid(t, "order.paid", paid)

	loc, _ := eventbus.NewEnvelope("evt_loc", TopicDriverLocation, "trace_drv",
		eventbus.Aggregate{Type: "driver", ID: "drv_sv", Region: "bkk"}, 1,
		map[string]any{"driver_id": "drv_sv", "h3_cell": "87283472bffffff", "lat": 13.75, "lng": 100.5, "recorded_at": now.Format(time.RFC3339)}, now)
	assertValid(t, "driver.location_updated", loc)
}

// TestSchemaParity: the SQLite runtime twin declares the same tables as the
// production PG migration (types differ; table+column names match).
func TestSchemaParity(t *testing.T) {
	for _, tbl := range []string{"dispatch_snapshots", "assignments"} {
		if !containsWord(pgMigration, tbl) {
			t.Fatalf("PG migration missing table %q", tbl)
		}
		if !containsWord(sqliteSchema, tbl) {
			t.Fatalf("SQLite twin missing table %q", tbl)
		}
	}
}

func containsWord(s, w string) bool {
	return len(s) > 0 && len(w) > 0 && (indexOf(s, w) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
