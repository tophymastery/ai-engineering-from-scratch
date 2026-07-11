package index

import (
	"fmt"
	"testing"
)

// TestSaltBalance_ChainMerchant is the D11 correctness property: a chain merchant
// indexed across the 16 salts `merchant_id#0..15` must spread its document load
// so the HOTTEST salt partition holds < 2× the mean. It builds a real 150k-item
// chain menu, routes every item document through the REAL SaltForDoc, and asserts
// on the actual histogram — no synthetic distribution.
func TestSaltBalance_ChainMerchant(t *testing.T) {
	const items = 150000
	merchantID := "mer_chain_celebrity_0000000000"

	docIDs := make([]string, items)
	for i := 0; i < items; i++ {
		docIDs[i] = fmt.Sprintf("itm_%s_%06d", merchantID, i)
	}
	hist := SaltHistogram(docIDs)

	mean := float64(items) / float64(NumSalts)
	max, min := 0, items
	for _, c := range hist {
		if c > max {
			max = c
		}
		if c < min {
			min = c
		}
	}
	ratio := float64(max) / mean
	t.Logf("chain merchant %q: %d items across %d salts — mean=%.1f hottest=%d (%.3f× mean) coldest=%d hist=%v",
		merchantID, items, NumSalts, mean, max, ratio, min, hist)

	if ratio >= 2.0 {
		t.Fatalf("hottest salt partition %d = %.3f× mean ≥ 2× (D11 salt-balance property FAILED)", max, ratio)
	}
	// Every partition must carry load (no dead salt).
	if min == 0 {
		t.Fatal("a salt partition received zero documents — salting is not spreading the chain")
	}
}

// TestSaltedKey checks the exact D11 wire key shape (`merchant_id#<0..15>`).
func TestSaltedKey(t *testing.T) {
	if got := SaltedKey("mer_abc", 0); got != "mer_abc#0" {
		t.Fatalf("SaltedKey salt 0 = %q want mer_abc#0", got)
	}
	if got := SaltedKey("mer_abc", 15); got != "mer_abc#15" {
		t.Fatalf("SaltedKey salt 15 = %q want mer_abc#15", got)
	}
	for i := 0; i < NumSalts; i++ {
		if s := SaltForDoc(fmt.Sprintf("itm_%d", i)); s < 0 || s >= NumSalts {
			t.Fatalf("SaltForDoc out of range: %d", s)
		}
	}
}
