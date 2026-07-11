package main

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// cmdValidate is the main CI gate (`contract-validate` stage). It runs the
// OpenAPI convention checks and the event-registry + D30 deprecation checks over
// the whole contracts root, printing a per-file report. Any violation returns an
// error (exit 1); the coordinator's pipeline treats that as a red merge gate.
func cmdValidate(root string) error {
	fmt.Printf("registryctl validate: %s\n", root)
	oaDir := filepath.Join(root, "openapi")
	evDir := filepath.Join(root, "events")

	nOA, err := validateOpenAPIDir(oaDir)
	if err != nil {
		return err
	}
	nEv, nTopics, err := validateEventsDir(evDir)
	if err != nil {
		return err
	}
	fmt.Printf("registryctl validate: OK — %d OpenAPI file(s), %d topic schema(s) across %d topic(s)\n", nOA, nEv, nTopics)
	return nil
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkSnakeProperties walks every nested "properties" map reachable from node
// and asserts each property NAME is snake_case (02 §1). It intentionally ignores
// schema/type names (keys under components.schemas), OpenAPI header names, etc. —
// only the keys of a "properties" object are field names.
func checkSnakeProperties(where string, node any) []string {
	var out []string
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok {
			for name, sub := range props {
				if !snakeCase.MatchString(name) {
					out = append(out, fmt.Sprintf("%s: field %q is not snake_case", where, name))
				}
				out = append(out, checkSnakeProperties(where, sub)...)
			}
		}
		for k, v := range n {
			if k == "properties" {
				continue
			}
			out = append(out, checkSnakeProperties(where, v)...)
		}
	case []any:
		for _, v := range n {
			out = append(out, checkSnakeProperties(where, v)...)
		}
	}
	return out
}
