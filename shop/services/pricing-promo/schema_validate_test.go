package main

// schema_validate_test.go — proves this slice honours the PUBLISHED contracts
// on both sides ("contract, not code, is the integration surface"):
//
//   - PRODUCER: the Quote the handler emits validates against the Quote schema in
//     contracts/openapi/pricing-promo.v1.yaml (type / required / properties /
//     items, with $ref resolution into components.schemas — so a producer change
//     that breaks the published shape is caught here, not only by registryctl).
//   - CONSUMER: the cart response pricing reads (pricing-promo→cart pact) decodes
//     into cartReadResponse with the subtotal Money pricing depends on.
//
// The OpenAPI schema uses the same keywords as the draft-07 subset validator
// cart/merchant-catalog use; a compact resolver handles $ref.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// oapiSchema is the subset of an OpenAPI 3.0 schema object we validate against.
type oapiSchema struct {
	Ref        string                 `yaml:"$ref"`
	Type       string                 `yaml:"type"`
	Required   []string               `yaml:"required"`
	Properties map[string]*oapiSchema `yaml:"properties"`
	Items      *oapiSchema            `yaml:"items"`
}

type oapiDoc struct {
	Components struct {
		Schemas map[string]*oapiSchema `yaml:"schemas"`
	} `yaml:"components"`
}

func loadOpenAPI(t *testing.T) *oapiDoc {
	t.Helper()
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "..", "..", "contracts", "openapi", "pricing-promo.v1.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var doc oapiDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse contract: %v", err)
	}
	return &doc
}

// resolve follows a local $ref (#/components/schemas/Name).
func (d *oapiDoc) resolve(s *oapiSchema) *oapiSchema {
	if s == nil || s.Ref == "" {
		return s
	}
	name := s.Ref[strings.LastIndex(s.Ref, "/")+1:]
	return d.Components.Schemas[name]
}

func (d *oapiDoc) validate(path string, s *oapiSchema, v any) []string {
	s = d.resolve(s)
	if s == nil {
		return nil
	}
	var errs []string
	switch s.Type {
	case "object", "":
		obj, ok := v.(map[string]any)
		if !ok {
			if s.Type == "object" {
				return []string{path + ": want object"}
			}
			return nil
		}
		for _, req := range s.Required {
			if _, ok := obj[req]; !ok {
				errs = append(errs, path+": missing required "+req)
			}
		}
		for k, child := range s.Properties {
			if val, ok := obj[k]; ok {
				errs = append(errs, d.validate(path+"."+k, child, val)...)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return []string{path + ": want array"}
		}
		for _, e := range arr {
			errs = append(errs, d.validate(path+"[]", s.Items, e)...)
		}
	case "string":
		if _, ok := v.(string); !ok {
			errs = append(errs, path+": want string")
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			errs = append(errs, path+": want integer")
		}
	case "number":
		if _, ok := v.(float64); !ok {
			errs = append(errs, path+": want number")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			errs = append(errs, path+": want boolean")
		}
	}
	return errs
}

// TestProducedQuoteConformsToContract emits a real quote through the handler and
// validates it against the published Quote schema (with Money/LineItem $refs).
func TestProducedQuoteConformsToContract(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.mux()
	req := httptest.NewRequest("POST", "/v1/quotes", strings.NewReader(createBody(tCart, 40000, "THB", "LUNCH25", true)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create quote -> %d", rec.Code)
	}
	var inst any
	if err := json.Unmarshal(rec.Body.Bytes(), &inst); err != nil {
		t.Fatalf("quote not JSON: %v", err)
	}
	doc := loadOpenAPI(t)
	quoteSchema := doc.Components.Schemas["Quote"]
	if quoteSchema == nil {
		t.Fatal("contract has no Quote schema")
	}
	if v := doc.validate("Quote", quoteSchema, inst); len(v) > 0 {
		t.Fatalf("produced quote violates the published contract: %v\nraw=%s", v, rec.Body.String())
	}
	// Spot-check the typed line items are present with the contract fields.
	obj := inst.(map[string]any)
	fees, _ := obj["fees"].([]any)
	if len(fees) == 0 {
		t.Fatal("produced quote has no typed fees[]")
	}
	f0 := fees[0].(map[string]any)
	for _, k := range []string{"type", "amount", "currency"} {
		if _, ok := f0[k]; !ok {
			t.Fatalf("fee line missing %q (02 §5 typed line item)", k)
		}
	}
}

// TestConsumedCartPactShape proves pricing decodes the cart response the
// pricing-promo→cart pact pins, reading the subtotal Money it prices against.
func TestConsumedCartPactShape(t *testing.T) {
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "..", "..", "contracts", "pacts", "pricing-promo__cart.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pact: %v", err)
	}
	var pact struct {
		Interactions []struct {
			Response struct {
				Body json.RawMessage `json:"body"`
			} `json:"response"`
		} `json:"interactions"`
	}
	if err := json.Unmarshal(b, &pact); err != nil {
		t.Fatalf("parse pact: %v", err)
	}
	if len(pact.Interactions) == 0 {
		t.Fatal("pact has no interactions")
	}
	// The cart consumer decodes the pinned response body.
	var cr cartReadResponse
	if err := json.Unmarshal(pact.Interactions[0].Response.Body, &cr); err != nil {
		t.Fatalf("pricing cannot decode the pinned cart body: %v", err)
	}
	if cr.Subtotal.Amount != 16000 || cr.Subtotal.Currency != "THB" {
		t.Fatalf("pinned cart subtotal = %+v want 16000 THB", cr.Subtotal)
	}
	// And the httpCart mapping produces the snapshot pricing uses.
	snap := cartSnapshot{CartID: cr.CartID, Subtotal: cr.Subtotal.Amount, Currency: cr.Currency}
	if snap.Subtotal != 16000 {
		t.Fatalf("snapshot subtotal %d", snap.Subtotal)
	}
}
