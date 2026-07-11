package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// scenarioDir points at the checked-in golden scenarios.
func scenarioDir() string { return filepath.Join("..", "..", "scenarios") }

func loadOrSkip(t *testing.T, file string) *Scenario {
	t.Helper()
	p := filepath.Join(scenarioDir(), file)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("scenario %s not present", p)
	}
	s, err := LoadScenario(p, file)
	if err != nil {
		t.Fatalf("load %s: %v", file, err)
	}
	return s
}

// TestByteIdenticalOnRerun is the S-T7 test criterion: same seed + scenario ⇒
// byte-identical dataset on rerun (hash compare of canonical dumps).
func TestByteIdenticalOnRerun(t *testing.T) {
	for _, file := range []string{"demo-small.yaml", "lunch-rush.yaml"} {
		s := loadOrSkip(t, file)
		a := Build(s).Canonical()
		b := Build(s).Canonical()
		ha, hb := sha256.Sum256(a), sha256.Sum256(b)
		if ha != hb {
			t.Fatalf("%s: dataset not byte-identical on rerun\n a=%x\n b=%x", file, ha, hb)
		}
		t.Logf("%s: byte-identical on rerun, sha256=%x (%d bytes)", file, ha, len(a))
	}
}

// TestSeedDrivesDataset guards that the seed actually matters (a constant-output
// bug would also pass the byte-identity test).
func TestSeedDrivesDataset(t *testing.T) {
	s := loadOrSkip(t, "demo-small.yaml")
	a := Build(s).Canonical()
	s.Seed++
	b := Build(s).Canonical()
	if sha256.Sum256(a) == sha256.Sum256(b) {
		t.Fatal("changing the seed produced an identical dataset — not seed-driven")
	}
}

// TestReferentialIntegrity: every order references a built user/merchant, and
// DELIVERED/DISPATCHED orders carry a driver.
func TestReferentialIntegrity(t *testing.T) {
	s := loadOrSkip(t, "lunch-rush.yaml")
	ds := Build(s)
	users := idSet(len(ds.Users), func(i int) string { return ds.Users[i].ID })
	merchants := idSet(len(ds.Merchants), func(i int) string { return ds.Merchants[i].ID })
	for _, o := range ds.Orders {
		if o.UserID != "" && !users[o.UserID] {
			t.Fatalf("order %s references unknown user %s", o.ID, o.UserID)
		}
		if o.MerchantID != "" && !merchants[o.MerchantID] {
			t.Fatalf("order %s references unknown merchant %s", o.ID, o.MerchantID)
		}
		if driverAssigned(o.Status) && o.DriverID == "" && len(ds.Drivers) > 0 {
			t.Fatalf("order %s in state %s has no driver", o.ID, o.Status)
		}
	}
	t.Logf("lunch-rush referential integrity OK: %d orders over %d users / %d merchants / %d drivers",
		len(ds.Orders), len(ds.Users), len(ds.Merchants), len(ds.Drivers))
}

func idSet(n int, get func(int) string) map[string]bool {
	m := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		m[get(i)] = true
	}
	return m
}
