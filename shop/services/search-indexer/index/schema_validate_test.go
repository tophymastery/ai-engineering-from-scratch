package index

// schema_validate_test.go — proves the events the search read model CONSUMES
// (menu.updated incl. the additive merchant_name/location, store.status_changed,
// rating.updated) are schema-valid against their published contracts
// (contracts/events/<topic>/v1.schema.json). Same compact, dependency-free
// draft-07 subset validator merchant-catalog uses for its EMITTED events — here
// applied from the consumer side so a contract break on any input topic reds the
// search build too. This is the "contract" test level for the consumed surface.

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
	Const                any                    `json:"const"`
	Required             []string               `json:"required"`
	Properties           map[string]*jsonSchema `json:"properties"`
	AdditionalProperties json.RawMessage        `json:"additionalProperties"`
}

func loadEventSchema(t *testing.T, topic string) *jsonSchema {
	t.Helper()
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "..", "..", "..", "contracts", "events", topic, "v1.schema.json")
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
	if s.Const != nil && fmt.Sprintf("%v", s.Const) != fmt.Sprintf("%v", v) {
		errs = append(errs, fmt.Sprintf("%s: value %v != const %v", path, v, s.Const))
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

// TestConsumedEventsAreSchemaValid builds one instance of each consumed event
// (as the neighbours emit them, incl. the additive menu.updated fields) and
// validates the full envelope against the published schema.
func TestConsumedEventsAreSchemaValid(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		topic   string
		payload any
	}{
		{TopicMenuUpdated, menuUpdatedPayload{
			MerchantID: "mer_schema", Version: 2, MenuETag: `"abc"`, MerchantName: "Schema Kitchen",
			Location: &geoPoint{Lat: 13.7563, Lng: 100.5018},
			Items:    []itemField{{ItemID: "itm_1", Name: "Som Tam", Amount: 8000, Currency: "THB", Available: true}},
		}},
		{TopicStoreStatus, storeStatusPayload{MerchantID: "mer_schema", Status: "OPEN", Version: 1, StatusETag: `"def"`}},
		{TopicRatingUpdated, ratingUpdatedPayload{MerchantID: "mer_schema", Rating: 4.6, RatingCount: 128, Version: 3}},
	}
	for _, c := range cases {
		env, err := eventbus.NewEnvelope("evt_"+c.topic, c.topic, "trace", eventbus.Aggregate{Type: "merchant", ID: "mer_schema", Region: "bkk"}, 1, c.payload, now)
		if err != nil {
			t.Fatalf("%s envelope: %v", c.topic, err)
		}
		raw, _ := env.Marshal()
		var inst any
		if err := json.Unmarshal(raw, &inst); err != nil {
			t.Fatalf("%s not JSON: %v", c.topic, err)
		}
		sch := loadEventSchema(t, c.topic)
		if v := sch.validate("$", inst); len(v) > 0 {
			t.Fatalf("consumed event on %s violates schema: %v\nraw=%s", c.topic, v, raw)
		}
	}
}
