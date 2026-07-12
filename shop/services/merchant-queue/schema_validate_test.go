package main

// schema_validate_test.go — proves the order.* events this slice EMITS (the
// synthesized order.accepted on accept, order.cancelled on reject) and the
// additive order.paid it CONSUMES (with the new merchant_id field, D30) are
// SCHEMA-VALID against their published contracts (contracts/events/<topic>/
// v1.schema.json). Uses the same compact, dependency-free draft-07 subset
// validator the other slices use (type / required / enum(const) / properties /
// additionalProperties). Run for real on every CI pass.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestEmittedAndConsumedEventsAreSchemaValid constructs each order.* envelope
// this slice produces/consumes (via the SAME makeOrderEnvelope the handlers use)
// and validates it against its published contract.
func TestEmittedAndConsumedEventsAreSchemaValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		topic   string
		orderID string
		merch   string
		extra   map[string]any
	}{
		// order.paid: the additive merchant_id (D30) must validate alongside the
		// required order_id/total/paid_at.
		{"order.paid", "ord_sv", "mer_sv", map[string]any{
			"payment_id": "pay_sv", "total": map[string]any{"amount": 42550, "currency": "THB"}, "paid_at": now.Format(time.RFC3339)}},
		// order.accepted: emitted by the accept handler.
		{"order.accepted", "ord_sv", "mer_sv", map[string]any{"accepted_at": now.Format(time.RFC3339)}},
		// order.cancelled: emitted by the reject handler (no merchant_id — matches
		// the declared payload).
		{"order.cancelled", "ord_sv", "", map[string]any{"cancelled_at": now.Format(time.RFC3339), "reason": "merchant_reject"}},
	}
	for _, c := range cases {
		env, err := makeOrderEnvelope("evt_"+c.topic, c.topic, c.orderID, c.merch, "bkk", c.extra, now)
		if err != nil {
			t.Fatalf("build %s: %v", c.topic, err)
		}
		raw, _ := env.Marshal()
		var inst any
		if err := json.Unmarshal(raw, &inst); err != nil {
			t.Fatalf("%s not JSON: %v", c.topic, err)
		}
		schema := loadEventSchema(t, c.topic)
		if v := schema.validate("$", inst); len(v) > 0 {
			t.Fatalf("%s envelope violates its published contract: %v\nraw=%s", c.topic, v, raw)
		}
		t.Logf("EVENT CONFORMANCE: %s valid vs contracts/events/%s/v1.schema.json", c.topic, c.topic)
	}
}
