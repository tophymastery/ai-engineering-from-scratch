package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustSchema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad schema json: %v", err)
	}
	return m
}

const baseSchema = `{
  "type":"object",
  "required":["payload"],
  "properties":{
    "event_type":{"const":"order.created"},
    "payload":{
      "type":"object",
      "required":["order_id","total"],
      "properties":{
        "order_id":{"type":"string"},
        "total":{"type":"object","properties":{"amount":{"type":"integer"}}},
        "item_count":{"type":"integer"}
      }
    }
  }
}`

func TestDiffAdditiveIsClean(t *testing.T) {
	newer := mustSchema(t, `{
      "type":"object","required":["payload"],
      "properties":{
        "event_type":{"const":"order.created"},
        "payload":{"type":"object","required":["order_id","total"],
          "properties":{
            "order_id":{"type":"string"},
            "total":{"type":"object","properties":{"amount":{"type":"integer"}}},
            "item_count":{"type":"integer"},
            "promo_code":{"type":"string"}
          }}}}`)
	if got := diffSchema("", mustSchema(t, baseSchema), newer); len(got) != 0 {
		t.Fatalf("additive change flagged breaking: %v", got)
	}
}

func TestDiffDetectsBreaking(t *testing.T) {
	cases := map[string]string{
		"removal/rename": `{"type":"object","required":["payload"],"properties":{"event_type":{"const":"order.created"},"payload":{"type":"object","required":["order_id","total"],"properties":{"order_id":{"type":"string"},"total":{"type":"object","properties":{"amount":{"type":"integer"}}}}}}}`,
		"type-change":    `{"type":"object","required":["payload"],"properties":{"event_type":{"const":"order.created"},"payload":{"type":"object","required":["order_id","total"],"properties":{"order_id":{"type":"string"},"total":{"type":"object","properties":{"amount":{"type":"integer"}}},"item_count":{"type":"string"}}}}}`,
		"required-add":   `{"type":"object","required":["payload"],"properties":{"event_type":{"const":"order.created"},"payload":{"type":"object","required":["order_id","total","item_count"],"properties":{"order_id":{"type":"string"},"total":{"type":"object","properties":{"amount":{"type":"integer"}}},"item_count":{"type":"integer"}}}}}`,
		"const-change":   `{"type":"object","required":["payload"],"properties":{"event_type":{"const":"order.updated"},"payload":{"type":"object","required":["order_id","total"],"properties":{"order_id":{"type":"string"},"total":{"type":"object","properties":{"amount":{"type":"integer"}}},"item_count":{"type":"integer"}}}}}`,
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			got := diffSchema("", mustSchema(t, baseSchema), mustSchema(t, s))
			if len(got) == 0 {
				t.Fatalf("%s: expected a breaking change, got none", name)
			}
		})
	}
}

func TestDiffMessagesMentionField(t *testing.T) {
	removed := mustSchema(t, `{"type":"object","properties":{"payload":{"type":"object","properties":{"order_id":{"type":"string"}}}}}`)
	got := diffSchema("", mustSchema(t, baseSchema), removed)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "item_count") || !strings.Contains(joined, "total") {
		t.Fatalf("expected removed fields named, got: %v", got)
	}
}
