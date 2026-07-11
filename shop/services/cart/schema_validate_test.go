package main

// schema_validate_test.go — proves cart CONSUMES the real published contract:
// the menu.updated envelope cart decodes validates against
// contracts/events/menu.updated/v1.schema.json (the same producer contract V-T3
// emits + the cart→merchant-catalog pact pins). Uses the same compact,
// dependency-free draft-07 subset validator merchant-catalog uses for its
// producer-side schema test (type / required / enum / properties /
// additionalProperties). This is the consumer half of the "contract, not code,
// is the integration surface" rule.

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

// TestConsumedMenuUpdatedIsSchemaValid builds the exact menu.updated envelope
// cart consumes and validates it against the published producer contract, then
// confirms the cart consumer decodes it — so a producer change that breaks the
// consume shape is caught here (not only by the cross-service pact/registry).
func TestConsumedMenuUpdatedIsSchemaValid(t *testing.T) {
	env := buildMenuUpdated(t, "evt_schema", tMerchant, tItem, "Som Tam", 8000, true, 1, time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	raw, err := env.Marshal()
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("event not JSON: %v", err)
	}
	schema := loadEventSchema(t, "menu.updated")
	if v := schema.validate("$", inst); len(v) > 0 {
		t.Fatalf("consumed menu.updated violates the published schema: %v\nraw=%s", v, raw)
	}

	// And the cart consumer decodes the same bytes into its payload struct.
	msg, err := eventbus.NewMessage(TopicMenuUpdated, env)
	if err != nil {
		t.Fatalf("new message: %v", err)
	}
	var p menuUpdatedPayload
	if err := json.Unmarshal(msg.Envelope.Payload, &p); err != nil {
		t.Fatalf("cart consumer failed to decode payload: %v", err)
	}
	if p.MerchantID != tMerchant || len(p.Items) != 1 || p.Items[0].Amount != 8000 {
		t.Fatalf("decoded payload mismatch: %+v", p)
	}
}
