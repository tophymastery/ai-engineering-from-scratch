package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// validateOpenAPIDir validates every *.yaml OpenAPI file under dir against the
// 02 §1 conventions used by the whole platform.
func validateOpenAPIDir(dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return 0, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return 0, fmt.Errorf("no OpenAPI files under %s", dir)
	}
	for _, f := range files {
		if err := validateOpenAPIFile(f); err != nil {
			return 0, err
		}
		fmt.Printf("  openapi OK: %s\n", filepath.Base(f))
	}
	return len(files), nil
}

func validateOpenAPIFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: parse: %w", path, err)
	}
	var problems []string

	// 1. Every path must be versioned under /v1/ (02 §1 path major version).
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		problems = append(problems, "no paths defined")
	}
	for p := range paths {
		if !strings.HasPrefix(p, "/v1/") {
			problems = append(problems, fmt.Sprintf("path %q is not under /v1/", p))
		}
	}

	// 2. snake_case field names across all schema properties (02 §1).
	problems = append(problems, checkSnakeProperties(filepath.Base(path), doc)...)

	// 3. The 02 §2 error envelope must be defined AND referenced. We require a
	//    components.schemas.Error with error.{code,trace_id,retryable} and at
	//    least one $ref to an Error response/schema somewhere in the document.
	if err := checkErrorEnvelope(doc); err != "" {
		problems = append(problems, err)
	}
	if !refsError(doc) {
		problems = append(problems, "no path references the error envelope (#/components/.../Error)")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s: %d convention violation(s):\n  - %s", path, len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// checkErrorEnvelope asserts components.schemas.Error matches the 02 §2 shape.
func checkErrorEnvelope(doc map[string]any) string {
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	errSchema, ok := schemas["Error"].(map[string]any)
	if !ok {
		return "components.schemas.Error is missing (02 §2 envelope)"
	}
	props, _ := errSchema["properties"].(map[string]any)
	inner, ok := props["error"].(map[string]any)
	if !ok {
		return "Error schema has no `error` object"
	}
	req := stringSet(inner["required"])
	for _, need := range []string{"code", "message", "trace_id", "retryable"} {
		if !req[need] {
			return fmt.Sprintf("Error.error is missing required field %q (02 §2)", need)
		}
	}
	return ""
}

// refsError reports whether any $ref in the document points at an Error schema
// or response — i.e. an endpoint actually wires the envelope in.
func refsError(node any) bool {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "$ref" {
				if s, ok := v.(string); ok && strings.Contains(s, "Error") {
					return true
				}
			}
			if refsError(v) {
				return true
			}
		}
	case []any:
		for _, v := range n {
			if refsError(v) {
				return true
			}
		}
	}
	return false
}
