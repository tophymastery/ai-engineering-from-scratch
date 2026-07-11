package index

import (
	"strconv"

	"github.com/shop-platform/shop/libs/sharding"
)

// --- salted merchant keys (D11) ---
//
// D11: "Merchant-keyed fan-out topics use salted keys `merchant_id#(0..15)` —
// per-salt ordering suffices for last-write-wins projections." A celebrity/chain
// merchant with a huge menu would otherwise pin every one of its document updates
// onto a single Kafka partition (the merchant_id key) and a single ingest worker,
// so a 150k-item update would hot-spot one partition. Salting spreads that
// merchant's document stream across NumSalts partitions; because the search
// projection is last-write-wins per document (each doc carries a monotonic
// version), per-salt ordering is sufficient — the global order across salts does
// not matter. See contracts/events/README-per-salt-ordering.md for the contract.

// NumSalts is the merchant fan-out salt count from D11 (`merchant_id#0..15`).
const NumSalts = 16

// SaltedKey is the Kafka partition key for one document of a merchant: it appends
// the doc's salt to the merchant id, `merchant_id#<0..15>`. This is the exact
// wire key D11 specifies.
func SaltedKey(merchantID string, salt int) string {
	return merchantID + "#" + strconv.Itoa(salt)
}

// SaltForDoc picks the salt partition for a single document (a menu item). It
// hashes the DOCUMENT id (not the merchant id) through the same finalized hash
// libs/sharding uses for shard routing, so a merchant's documents spread evenly
// across the 16 salts. Deterministic: the same doc always lands on the same salt,
// which is what makes per-salt ordering a stable, replayable guarantee.
func SaltForDoc(docID string) int {
	return int(sharding.Hash64(docID) % uint64(NumSalts))
}

// SaltHistogram counts how many of the given document ids land on each of the 16
// salt partitions. Exposed so the load-balance property (hottest partition < 2×
// mean) can be measured on a real 150k-item chain merchant (salt_test.go).
func SaltHistogram(docIDs []string) [NumSalts]int {
	var h [NumSalts]int
	for _, id := range docIDs {
		h[SaltForDoc(id)]++
	}
	return h
}
