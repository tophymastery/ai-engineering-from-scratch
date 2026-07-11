package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// inventory.go — the data-inventory + retention register validator. Enforces the
// D3 rule: every PII-bearing column in every migration MUST be declared in the
// checked-in data-inventory.yaml, and every PII class MUST have a retention
// entry. An unregistered PII column (the "unregistered-table fixture") turns CI
// RED. This is the machine-readable, CI-validated register D3 asks for ("like
// the log schema").

type inventoryDoc struct {
	Version int    `yaml:"version"`
	Service string `yaml:"service"`
	Decision string `yaml:"decision"`
	Stores  []struct {
		Name      string `yaml:"name"`
		Residency string `yaml:"residency"`
		Backup    string `yaml:"backup"`
		Columns   []struct {
			Column    string `yaml:"column"`
			Class     string `yaml:"class"`
			Encrypted bool   `yaml:"encrypted"`
		} `yaml:"columns"`
	} `yaml:"stores"`
}

type retentionDoc struct {
	Version int    `yaml:"version"`
	Service string `yaml:"service"`
	Erasure struct {
		Mechanism string   `yaml:"mechanism"`
		Key       string   `yaml:"key"`
		SLAHours  int      `yaml:"sla_hours"`
		Scope     []string `yaml:"scope"`
	} `yaml:"erasure"`
	Classes []struct {
		Class     string `yaml:"class"`
		Basis     string `yaml:"basis"`
		Retention string `yaml:"retention"`
		Erasure   string `yaml:"erasure"`
	} `yaml:"classes"`
}

func loadInventory(path string) (inventoryDoc, error) {
	var d inventoryDoc
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	err = yaml.Unmarshal(b, &d)
	return d, err
}

func loadRetention(path string) (retentionDoc, error) {
	var d retentionDoc
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	err = yaml.Unmarshal(b, &d)
	return d, err
}

// piiColumn is a PII column DECLARED by a migration (via a `-- pii:<class>`
// marker or a `*_ct` ciphertext column name).
type piiColumn struct {
	Table  string
	Column string
	Class  string // from the marker; "" if only inferred from the _ct suffix
}

var (
	reCreate = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([A-Za-z0-9_]+)"?\s*\(`)
	reColPII = regexp.MustCompile(`^\s*"?([A-Za-z0-9_]+)"?\s+[A-Za-z].*?--\s*pii:([A-Za-z0-9_]+)`)
	reColCT  = regexp.MustCompile(`^\s*"?([A-Za-z0-9_]+_ct)"?\s+`)
)

// parseMigrationPII extracts every declared PII column from one migration's SQL.
func parseMigrationPII(sql string) []piiColumn {
	var cols []piiColumn
	table := ""
	for _, line := range strings.Split(sql, "\n") {
		if m := reCreate.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		if table == "" {
			continue
		}
		if m := reColPII.FindStringSubmatch(line); m != nil {
			cols = append(cols, piiColumn{Table: table, Column: m[1], Class: m[2]})
			continue
		}
		if m := reColCT.FindStringSubmatch(line); m != nil {
			// A _ct column without a pii: marker — still PII, class unknown.
			cols = append(cols, piiColumn{Table: table, Column: m[1], Class: ""})
		}
	}
	return cols
}

// checkInventory verifies every declared PII column across the given migration
// files is registered in the inventory, and every registered class has a
// retention entry. Returns human-readable violations (empty => clean).
func checkInventory(migrationFiles []string, inv inventoryDoc, ret retentionDoc) ([]string, error) {
	// Registered (table.column) -> class from the inventory.
	registered := map[string]string{}
	for _, s := range inv.Stores {
		for _, c := range s.Columns {
			registered[s.Name+"."+c.Column] = c.Class
		}
	}
	// Retention classes present.
	retClasses := map[string]bool{}
	for _, c := range ret.Classes {
		retClasses[c.Class] = true
	}

	var violations []string
	seen := map[string]bool{}
	for _, path := range migrationFiles {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, col := range parseMigrationPII(string(b)) {
			key := col.Table + "." + col.Column
			seen[key] = true
			regClass, ok := registered[key]
			if !ok {
				violations = append(violations,
					fmt.Sprintf("UNREGISTERED PII column %s (class=%q, in %s) — not in data-inventory.yaml", key, col.Class, path))
				continue
			}
			if col.Class != "" && regClass != col.Class {
				violations = append(violations,
					fmt.Sprintf("PII column %s class mismatch: migration says %q, inventory says %q", key, col.Class, regClass))
			}
			if regClass != "" && !retClasses[regClass] {
				violations = append(violations,
					fmt.Sprintf("PII class %q (column %s) has NO retention register entry", regClass, key))
			}
		}
	}
	// Reverse check: a registered column that no longer exists in the migrations
	// is stale drift.
	for key := range registered {
		if !seen[key] {
			violations = append(violations, fmt.Sprintf("STALE inventory entry %s — no such PII column in the scanned migrations", key))
		}
	}
	sort.Strings(violations)
	return violations, nil
}

// validateRegisters checks the two registers are internally well-formed (the
// "CI-validated register" DoD item): required fields present, erasure SLA set,
// every inventory class resolvable in retention.
func validateRegisters(inv inventoryDoc, ret retentionDoc) []string {
	var v []string
	if inv.Version == 0 {
		v = append(v, "data-inventory.yaml: missing version")
	}
	if inv.Service == "" {
		v = append(v, "data-inventory.yaml: missing service")
	}
	if len(inv.Stores) == 0 {
		v = append(v, "data-inventory.yaml: no stores declared")
	}
	for _, s := range inv.Stores {
		if s.Residency == "" {
			v = append(v, fmt.Sprintf("data-inventory.yaml: store %q missing residency", s.Name))
		}
		for _, c := range s.Columns {
			if c.Class == "" {
				v = append(v, fmt.Sprintf("data-inventory.yaml: %s.%s missing class", s.Name, c.Column))
			}
			if !c.Encrypted {
				v = append(v, fmt.Sprintf("data-inventory.yaml: PII column %s.%s must be encrypted:true (D3)", s.Name, c.Column))
			}
		}
	}
	if ret.Erasure.Mechanism != "crypto-shredding" {
		v = append(v, "retention-register.yaml: erasure.mechanism must be crypto-shredding (D3)")
	}
	if ret.Erasure.SLAHours <= 0 || ret.Erasure.SLAHours > 72 {
		v = append(v, fmt.Sprintf("retention-register.yaml: erasure.sla_hours must be 1..72 (D3), got %d", ret.Erasure.SLAHours))
	}
	retClasses := map[string]bool{}
	for _, c := range ret.Classes {
		retClasses[c.Class] = true
	}
	for _, s := range inv.Stores {
		for _, c := range s.Columns {
			if c.Class != "" && !retClasses[c.Class] {
				v = append(v, fmt.Sprintf("class %q (%s.%s) has no retention entry", c.Class, s.Name, c.Column))
			}
		}
	}
	sort.Strings(v)
	return v
}
