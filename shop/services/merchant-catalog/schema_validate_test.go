package main

// schema_validate_test.go — proves every event this slice emits is SCHEMA-VALID
// against its published contract (contracts/events/<topic>/v1.schema.json). Uses
// the same compact, dependency-free draft-07 subset validator as the log-schema
// test (libs/logging/schema_test.go): type / required / enum / properties /
// additionalProperties. This is the "schema-valid events" DoD proof, run for
// real on every CI pass.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type jsonSchema struct {
	Type                 any                    `json:"type"`
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
	case "null":
		return v == nil
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
		for _, t := range types {
			if jsTypeMatches(t, v) {
				ok = true
				break
			}
		}
		if !ok {
			return []string{fmt.Sprintf("%s: type mismatch (want %v)", path, types)}
		}
	}
	if len(s.Enum) > 0 {
		found := false
		for _, e := range s.Enum {
			if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", v) {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: value %v not in enum", path, v))
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

// TestEmittedEventsAreSchemaValid drives real mutations, then validates EVERY
// outbox event against its published JSON schema.
func TestEmittedEventsAreSchemaValid(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()
	mid := "mer_01test000000000000000schema"
	menuETag, statusETag := createMerchant(t, h, mid)
	do(t, h, http.MethodPatch, "/v1/merchants/"+mid+"/menu", menuETag,
		`{"upsert_items":[{"name":"Pad Thai","price":{"amount":7500,"currency":"THB"},"available":true}]}`)
	do(t, h, http.MethodPut, "/v1/merchants/"+mid+"/store-status", statusETag, `{"status":"OPEN"}`)

	menuSchema := loadEventSchema(t, "menu.updated")
	statusSchema := loadEventSchema(t, "store.status_changed")

	recs, err := s.st.ob.Tail(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("want 4 events, got %d", len(recs))
	}
	for _, r := range recs {
		var inst any
		if err := json.Unmarshal(r.Raw, &inst); err != nil {
			t.Fatalf("event not JSON: %v", err)
		}
		var sch *jsonSchema
		switch r.Topic {
		case "menu.updated":
			sch = menuSchema
		case "store.status_changed":
			sch = statusSchema
		default:
			t.Fatalf("unexpected topic %q", r.Topic)
		}
		if v := sch.validate("$", inst); len(v) > 0 {
			t.Fatalf("event on %s violates schema: %v\nraw=%s", r.Topic, v, r.Raw)
		}
	}
}
