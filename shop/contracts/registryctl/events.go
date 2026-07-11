package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// envelopeRequired are the 02 §4.3 envelope fields every topic schema must carry.
var envelopeRequired = []string{
	"event_id", "event_type", "occurred_at", "trace_id", "aggregate", "schema_version", "payload",
}

// vSuffix matches a topic dir that is an explicit new-version topic, e.g.
// "order.paid.v2", "order.paid.v3". Group 1 is the base topic name.
var vSuffix = regexp.MustCompile(`^(.+)\.v([2-9]|[1-9][0-9]+)$`)

// validateEventsDir validates every event topic under dir: envelope conformance,
// snake_case payload fields, and the D30 dual-publish/deprecation rule. Returns
// (schema count, topic count).
func validateEventsDir(dir string) (int, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	topics := map[string]bool{} // topic dirs that actually hold schemas
	nSchemas := 0
	var topicNames []string
	for _, e := range entries {
		if !e.IsDir() {
			continue // envelope.schema.json etc. live at the top level
		}
		topic := e.Name()
		schemaFiles, _ := filepath.Glob(filepath.Join(dir, topic, "*.schema.json"))
		if len(schemaFiles) == 0 {
			continue
		}
		topics[topic] = true
		topicNames = append(topicNames, topic)
		sort.Strings(schemaFiles)
		for _, sf := range schemaFiles {
			if err := validateTopicSchema(topic, sf); err != nil {
				return 0, 0, err
			}
			nSchemas++
			fmt.Printf("  event OK: %s/%s\n", topic, filepath.Base(sf))
		}
	}
	sort.Strings(topicNames)

	// D30: a `<base>.vN` topic requires the base topic to carry a deprecation.yaml
	// with a valid, not-yet-past deprecation_date pointing at the new topic.
	for _, topic := range topicNames {
		m := vSuffix.FindStringSubmatch(topic)
		if m == nil {
			continue
		}
		base := m[1]
		if !topics[base] {
			return 0, 0, fmt.Errorf("D30: new-version topic %q exists but its base topic %q is absent", topic, base)
		}
		if err := checkDeprecation(dir, base, topic); err != nil {
			return 0, 0, err
		}
		fmt.Printf("  d30 OK: %s deprecates %s (dual-publish window valid)\n", topic, base)
	}
	return nSchemas, len(topicNames), nil
}

// validateTopicSchema asserts a topic schema conforms to the 02 §4.3 envelope
// and uses snake_case payload fields.
func validateTopicSchema(topic, path string) error {
	s, err := loadJSON(path)
	if err != nil {
		return err
	}
	var problems []string

	// envelope required set.
	req := stringSet(s["required"])
	for _, f := range envelopeRequired {
		if !req[f] {
			problems = append(problems, fmt.Sprintf("envelope required field %q missing from schema.required", f))
		}
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		problems = append(problems, "schema has no properties")
	}

	// event_type must be a const equal to the topic (dir) name.
	if et, _ := props["event_type"].(map[string]any); et != nil {
		if c, ok := et["const"].(string); !ok || c != topic {
			problems = append(problems, fmt.Sprintf("event_type const must equal topic %q (got %v)", topic, et["const"]))
		}
	} else {
		problems = append(problems, "event_type must be a const")
	}

	// payload must be an object.
	if pl, _ := props["payload"].(map[string]any); pl != nil {
		if t, _ := pl["type"].(string); t != "object" {
			problems = append(problems, "payload must be type object")
		}
		problems = append(problems, checkSnakeProperties(topic, pl)...)
	} else {
		problems = append(problems, "payload is missing")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s: %d envelope violation(s):\n  - %s", path, len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// checkDeprecation enforces the D30 deprecation record on the base topic.
func checkDeprecation(eventsDir, base, newTopic string) error {
	dp := filepath.Join(eventsDir, base, "deprecation.yaml")
	b, err := os.ReadFile(dp)
	if err != nil {
		return fmt.Errorf("D30: %q has replacement %q but no %s/deprecation.yaml (topic, replaced_by, deprecation_date required)", base, newTopic, base)
	}
	var d struct {
		Topic           string `yaml:"topic"`
		ReplacedBy      string `yaml:"replaced_by"`
		DeprecationDate string `yaml:"deprecation_date"`
	}
	if err := yaml.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("%s: parse: %w", dp, err)
	}
	if d.Topic != base {
		return fmt.Errorf("%s: topic %q must equal %q", dp, d.Topic, base)
	}
	if d.ReplacedBy != newTopic {
		return fmt.Errorf("%s: replaced_by %q must equal %q", dp, d.ReplacedBy, newTopic)
	}
	if d.DeprecationDate == "" {
		return fmt.Errorf("%s: deprecation_date is required (D30 enforced date)", dp)
	}
	when, err := time.Parse("2006-01-02", d.DeprecationDate)
	if err != nil {
		return fmt.Errorf("%s: deprecation_date %q is not YYYY-MM-DD: %w", dp, d.DeprecationDate, err)
	}
	if when.Before(time.Now().Truncate(24 * time.Hour)) {
		return fmt.Errorf("%s: deprecation_date %s has PASSED — old topic %q must be removed (D30 enforced deprecation)", dp, d.DeprecationDate, base)
	}
	return nil
}
