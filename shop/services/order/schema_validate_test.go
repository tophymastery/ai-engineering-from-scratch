package main

// schema_validate_test.go proves this slice honours the PUBLISHED contracts
// ("contract, not code, is the integration surface"):
//
//   - PRODUCER (events): EVERY order.* event the saga emits validates against its
//     draft-07 topic schema in contracts/events/<topic>/v1.schema.json (envelope
//     required fields, event_type/aggregate const, payload required fields,
//     additionalProperties). So a producer change that breaks a published event
//     shape is caught here, not only by registryctl.
//   - PRODUCER (HTTP): the Order response the handler emits validates against the
//     Order schema in contracts/openapi/order.v1.yaml.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// --- draft-07 JSON-schema subset validator (events) -------------------------

type jsonSchema struct {
	Type                 string                 `json:"type"`
	Required             []string               `json:"required"`
	Properties           map[string]*jsonSchema `json:"properties"`
	Items                *jsonSchema            `json:"items"`
	Const                any                    `json:"const"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
}

func loadEventSchema(t *testing.T, topic string) *jsonSchema {
	t.Helper()
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "..", "..", "contracts", "events", topic, "v1.schema.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event schema %s: %v", topic, err)
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("parse event schema %s: %v", topic, err)
	}
	return &s
}

func validateJSON(path string, s *jsonSchema, v any) []string {
	if s == nil {
		return nil
	}
	var errs []string
	if s.Const != nil {
		if v != s.Const {
			errs = append(errs, path+": const mismatch (got "+toStr(v)+" want "+toStr(s.Const)+")")
		}
	}
	switch s.Type {
	case "object", "":
		obj, ok := v.(map[string]any)
		if !ok {
			if s.Type == "object" {
				return []string{path + ": want object"}
			}
			return errs
		}
		for _, req := range s.Required {
			if _, ok := obj[req]; !ok {
				errs = append(errs, path+": missing required "+req)
			}
		}
		if s.AdditionalProperties != nil && !*s.AdditionalProperties {
			for k := range obj {
				if _, ok := s.Properties[k]; !ok {
					errs = append(errs, path+": additional property "+k+" not allowed")
				}
			}
		}
		for k, child := range s.Properties {
			if val, ok := obj[k]; ok {
				errs = append(errs, validateJSON(path+"."+k, child, val)...)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return []string{path + ": want array"}
		}
		for _, e := range arr {
			errs = append(errs, validateJSON(path+"[]", s.Items, e)...)
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
	}
	return errs
}

func toStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestEmittedEventsConformToContracts drives one order through EVERY state
// (SETTLED via the happy path + a second order to CANCELLED) and validates each
// emitted outbox envelope against its published topic schema.
func TestEmittedEventsConformToContracts(t *testing.T) {
	srv, clk, _ := newTestServer(t)
	h := srv.mux()
	ctx := context.Background()

	// Order 1: full happy path to SETTLED.
	id := checkout(t, srv, "k1")
	inject(t, srv, id, "payment.authorized", "e1")
	do(t, h, "POST", "/v1/orders/"+id+":accept", "{}", "")
	inject(t, srv, id, "dispatch.assigned", "e2")
	inject(t, srv, id, "driver.picked_up", "e3")
	inject(t, srv, id, "driver.delivered", "e4")
	clk.Advance(DefaultCaptureByWindow + 60_000_000_000) // +30m1s
	srv.sweeper.SweepOnce(ctx)

	// Order 2: to CANCELLED (so order.cancelled is emitted too).
	id2 := checkout(t, srv, "k2")
	do(t, h, "POST", "/v1/orders/"+id2+":cancel", "{}", "")

	// Read every emitted outbox event and validate against its topic schema.
	rows, err := srv.st.db.QueryContext(ctx, `SELECT topic, payload FROM outbox ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	count := 0
	for rows.Next() {
		var topic string
		var payload []byte
		if err := rows.Scan(&topic, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var env any
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("event %s not JSON: %v", topic, err)
		}
		schema := loadEventSchema(t, topic)
		if v := validateJSON(topic, schema, env); len(v) > 0 {
			t.Fatalf("emitted %s violates its published contract: %v\nraw=%s", topic, v, payload)
		}
		seen[topic] = true
		count++
	}
	// Every lifecycle topic must have been exercised.
	for _, topic := range []string{"order.created", "order.paid", "order.accepted", "order.dispatched", "order.picked_up", "order.delivered", "order.settled", "order.cancelled"} {
		if !seen[topic] {
			t.Fatalf("no %s event emitted (topic coverage gap)", topic)
		}
	}
	t.Logf("EVENT CONFORMANCE: %d emitted events across %d order.* topics all valid vs published schemas", count, len(seen))
}

// --- OpenAPI Order response conformance -------------------------------------

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
		arr, _ := v.([]any)
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
	}
	return errs
}

// TestProducedOrderConformsToContract emits a real Order through the handler and
// validates it against the published Order schema (with Money $ref).
func TestProducedOrderConformsToContract(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(checkoutBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "conf-1")
	rec := httptest.NewRecorder()
	srv.mux().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("checkout -> %d", rec.Code)
	}
	var inst any
	if err := json.Unmarshal(rec.Body.Bytes(), &inst); err != nil {
		t.Fatalf("order not JSON: %v", err)
	}
	wd, _ := os.Getwd()
	b, err := os.ReadFile(filepath.Join(wd, "..", "..", "contracts", "openapi", "order.v1.yaml"))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var doc oapiDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse contract: %v", err)
	}
	orderSchema := doc.Components.Schemas["Order"]
	if orderSchema == nil {
		t.Fatal("contract has no Order schema")
	}
	if v := doc.validate("Order", orderSchema, inst); len(v) > 0 {
		t.Fatalf("produced order violates the published contract: %v\nraw=%s", v, rec.Body.String())
	}
}
