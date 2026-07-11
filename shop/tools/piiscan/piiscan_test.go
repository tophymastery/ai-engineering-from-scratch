package main

import (
	"path/filepath"
	"testing"
)

const (
	realInventory = "../../services/identity-profile/data-inventory.yaml"
	realRetention = "../../services/identity-profile/retention-register.yaml"
	realMigration = "../../services/identity-profile/migrations/0001_profile.pg.sql"
)

// --- scan-traffic: both directions ---

func TestScanTrafficCleanIsGreen(t *testing.T) {
	f, err := scanTraffic([]string{"testdata/clean-traffic.jsonl"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 0 {
		t.Fatalf("token-only traffic should be clean, got %d findings: %v", len(f), f)
	}
}

func TestScanTrafficLeakIsRed(t *testing.T) {
	// Structural detectors fire without any known list (email + intl phone).
	f, err := scanTraffic([]string{"testdata/leaky-traffic.jsonl"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) == 0 {
		t.Fatal("leaky traffic must produce findings (email/phone)")
	}
	kinds := map[string]bool{}
	for _, x := range f {
		kinds[x.Kind] = true
	}
	if !kinds["email"] || !kinds["phone"] {
		t.Fatalf("expected email+phone detections, got %v", kinds)
	}
}

func TestScanTrafficKnownWordlistIsRed(t *testing.T) {
	known := []string{"Nguyen Van An", "budi.santoso@example.co.id"}
	f, err := scanTraffic([]string{"testdata/leaky-traffic.jsonl"}, known)
	if err != nil {
		t.Fatal(err)
	}
	sawKnown := false
	for _, x := range f {
		if x.Kind == "known-pii" {
			sawKnown = true
		}
	}
	if !sawKnown {
		t.Fatalf("known-PII wordlist should catch the leaked name, findings=%v", f)
	}
}

// A Luhn-valid card must be flagged, but a 9-digit nanosecond timestamp must NOT.
func TestCardDetectorPrecise(t *testing.T) {
	if !luhn("4111 1111 1111 1111") {
		t.Fatal("valid test PAN should be Luhn-detected")
	}
	if luhn("753266363") { // a timestamp fraction — not a card
		t.Fatal("9-digit timestamp fraction must not be flagged as a card")
	}
}

// --- check-inventory: both directions ---

func TestInventoryRegisteredIsGreen(t *testing.T) {
	inv, err := loadInventory(realInventory)
	if err != nil {
		t.Fatal(err)
	}
	ret, err := loadRetention(realRetention)
	if err != nil {
		t.Fatal(err)
	}
	v, err := checkInventory([]string{realMigration}, inv, ret)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("real migrations should be fully registered, violations: %v", v)
	}
}

func TestUnregisteredTableIsRed(t *testing.T) {
	inv, _ := loadInventory(realInventory)
	ret, _ := loadRetention(realRetention)
	// Real migration (clean) + the unregistered-table fixture (must fail).
	v, err := checkInventory([]string{realMigration, "testdata/unregistered.sql"}, inv, ret)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Fatal("unregistered PII table must be flagged (unregistered-table => CI red)")
	}
	found := false
	for _, s := range v {
		if filepath.Base("testdata/unregistered.sql") != "" && contains(s, "marketing_leads.full_name") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected marketing_leads.full_name flagged, got: %v", v)
	}
}

func TestParseMigrationPII(t *testing.T) {
	cols := parseMigrationPII(`CREATE TABLE profiles (
	    user_token TEXT PRIMARY KEY,
	    full_name_ct TEXT, -- pii:name
	    phone_ct TEXT -- pii:phone
	);`)
	if len(cols) != 2 {
		t.Fatalf("expected 2 PII cols, got %d: %v", len(cols), cols)
	}
	if cols[0].Table != "profiles" || cols[0].Column != "full_name_ct" || cols[0].Class != "name" {
		t.Fatalf("bad parse: %+v", cols[0])
	}
}

// --- validate: registers well-formed ---

func TestValidateRegistersGreen(t *testing.T) {
	inv, _ := loadInventory(realInventory)
	ret, _ := loadRetention(realRetention)
	if v := validateRegisters(inv, ret); len(v) != 0 {
		t.Fatalf("registers should be well-formed, problems: %v", v)
	}
	if ret.Erasure.SLAHours > 72 || ret.Erasure.Mechanism != "crypto-shredding" {
		t.Fatalf("erasure register wrong: %+v", ret.Erasure)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
