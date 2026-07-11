package main

import (
	"encoding/json"
	"testing"
)

func TestPathToRegexParamsAndActionVerb(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/v1/orders/{order_id}", "/v1/orders/ord_123", true},
		{"/v1/orders/{order_id}", "/v1/orders/ord_123/extra", false},
		{"/v1/orders/{order_id}:cancel", "/v1/orders/ord_123:cancel", true},
		{"/v1/orders/{order_id}:cancel", "/v1/orders/ord_123", false},
		{"/v1/orders/{order_id}", "/v1/orders/ord_123:cancel", false}, // ':' excluded from param
		{"/v1/quotes", "/v1/quotes", true},
	}
	for _, c := range cases {
		got := pathToRegex(c.pattern).MatchString(c.path)
		if got != c.want {
			t.Errorf("pathToRegex(%q).Match(%q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestSynthResolvesRefsAndTypes(t *testing.T) {
	var doc map[string]any
	_ = json.Unmarshal([]byte(`{
      "components":{"schemas":{
        "Money":{"type":"object","properties":{"amount":{"type":"integer"},"currency":{"type":"string"}}},
        "Order":{"type":"object","properties":{
          "order_id":{"type":"string"},
          "status":{"type":"string","enum":["PAID","CANCELLED"]},
          "total":{"$ref":"#/components/schemas/Money"}
        }}
      }}
    }`), &doc)
	sch := map[string]any{"$ref": "#/components/schemas/Order"}
	out := synth(doc, sch, 0).(map[string]any)
	if out["order_id"] != "string" {
		t.Errorf("order_id synth = %v, want \"string\"", out["order_id"])
	}
	if out["status"] != "PAID" {
		t.Errorf("status synth = %v, want first enum PAID", out["status"])
	}
	money, ok := out["total"].(map[string]any)
	if !ok || money["amount"] != 0 || money["currency"] != "string" {
		t.Errorf("total synth via $ref = %v", out["total"])
	}
}

func TestExamplePreferredOverSchema(t *testing.T) {
	var op map[string]any
	_ = json.Unmarshal([]byte(`{
      "responses":{
        "201":{"content":{"application/json":{
          "schema":{"type":"object","properties":{"x":{"type":"string"}}},
          "example":{"order_id":"ord_1","status":"PAYMENT_PENDING"}
        }}},
        "default":{"content":{"application/json":{"schema":{"type":"object"}}}}
      }}`), &op)
	status, body := exampleResponse(map[string]any{}, op)
	if status != 201 {
		t.Fatalf("status = %d, want 201", status)
	}
	m := body.(map[string]any)
	if m["order_id"] != "ord_1" {
		t.Fatalf("expected example body, got %v", body)
	}
}
