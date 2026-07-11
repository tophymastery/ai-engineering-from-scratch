package eventbus

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// envelopeSchemaJSON is the canonical 02 §4.3 envelope schema, embedded so the
// bus can validate messages with no filesystem dependency. schema_drift_test.go
// asserts this copy is byte-identical to contracts/events/envelope.schema.json
// (the single source of truth) so it can never silently drift.
//
//go:embed envelope.schema.json
var envelopeSchemaJSON []byte

var (
	envSchemaOnce sync.Once
	envSchema     *schemaNode
	envSchemaErr  error
)

func envelopeSchema() (*schemaNode, error) {
	envSchemaOnce.Do(func() {
		envSchema, envSchemaErr = compileSchema(envelopeSchemaJSON)
	})
	return envSchema, envSchemaErr
}

// ValidateEnvelope validates raw wire bytes against the embedded envelope
// schema. It is called once per event at the outbox WriteInTx ingress boundary
// (every event validated exactly once, not re-validated in the hot delivery
// path). Returns a descriptive error naming the first violated field.
func ValidateEnvelope(raw []byte) error {
	s, err := envelopeSchema()
	if err != nil {
		return fmt.Errorf("eventbus: envelope schema compile: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("eventbus: envelope is not valid JSON: %w", err)
	}
	return s.validate(v, "$")
}

// --- Minimal JSON Schema (draft-07 subset) sufficient for the envelope ---
// Supports: type (object/string/integer/number/boolean/array), required,
// properties, nested objects. This is intentionally not a general validator;
// it covers exactly the keywords the envelope contract uses so the library
// carries no third-party schema dependency (matching the repo's build story).

type schemaNode struct {
	typ      string
	required []string
	props    map[string]*schemaNode
}

func compileSchema(b []byte) (*schemaNode, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return compileNode(m)
}

func compileNode(m map[string]any) (*schemaNode, error) {
	n := &schemaNode{props: map[string]*schemaNode{}}
	if t, ok := m["type"].(string); ok {
		n.typ = t
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				n.required = append(n.required, s)
			}
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for k, v := range props {
			pm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			cn, err := compileNode(pm)
			if err != nil {
				return nil, err
			}
			n.props[k] = cn
		}
	}
	return n, nil
}

func (n *schemaNode) validate(v any, path string) error {
	switch n.typ {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object, got %T", path, v)
		}
		for _, r := range n.required {
			if _, present := obj[r]; !present {
				return fmt.Errorf("%s: missing required field %q", path, r)
			}
		}
		for k, cn := range n.props {
			if cv, present := obj[k]; present {
				if err := cn.validate(cv, path+"."+k); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: expected string, got %T", path, v)
		}
	case "integer":
		// JSON numbers decode to float64; accept integral values.
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("%s: expected integer, got %T", path, v)
		}
		if f != float64(int64(f)) {
			return fmt.Errorf("%s: expected integer, got fractional %v", path, f)
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("%s: expected number, got %T", path, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %T", path, v)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("%s: expected array, got %T", path, v)
		}
	case "":
		// no type constraint at this node
	}
	return nil
}
