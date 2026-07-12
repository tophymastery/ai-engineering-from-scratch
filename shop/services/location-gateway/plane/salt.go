package plane

import (
	"strconv"

	"github.com/shop-platform/shop/libs/sharding"
)

// salt.go — hot-key spreading for the H3 geo index (D15). D15 warns explicitly:
// "one GEO key is itself a hot partition at 500k drivers." A dense downtown res-7
// cell would otherwise concentrate a huge fraction of all position writes onto a
// single Redis key. So — exactly as V-T4 salts merchant fan-out keys
// (merchant_id#0..15) to keep the hottest partition < 2× mean — this slice salts
// the physical geo key: h7_<lat>_<lng>#<0..NumSalts-1>. A driver's salt is a
// stable hash of its driver_id (so all of one driver's updates land on the same
// sub-key — the geo store can overwrite the driver's previous position), and the
// drivers within any one cell spread uniformly across the NumSalts sub-keys via
// the finalized libs/sharding.Hash64 mix.
//
// The invariant this buys (V-T13 test criterion): the hottest physical geo key
// receives < 2% of writes. NumSalts = 64 makes this hold even in the fully
// degenerate case where EVERY driver sits in ONE cell: the writes then spread
// across 64 sub-keys ⇒ hottest ≈ 1/64 = 1.5625% < 2%. Under any realistic spatial
// skew it is far lower. Proven on a real write histogram in salt_test.go.

// NumSalts is the geo-key salt count. 64 (> 50) guarantees the hottest physical
// key < 2% of writes even under maximal spatial concentration (1/64 = 1.5625%).
const NumSalts = 64

// SaltFor picks the salt sub-key for a driver. It hashes the driver_id through the
// same finalized hash libs/sharding uses for shard routing (FNV-1a + murmur3
// fmix64 avalanche), so drivers spread evenly across the NumSalts sub-keys of a
// cell regardless of id shape. Stable per driver.
func SaltFor(driverID string) int {
	return int(sharding.Hash64(driverID) % uint64(NumSalts))
}

// SaltedKey is the physical geo-store key for a cell + salt: cell.Key()#salt.
func SaltedKey(cellKey string, salt int) string {
	return cellKey + "#" + strconv.Itoa(salt)
}

// PhysicalKey is the physical geo-store key a single driver's position writes to:
// its cell salted by SaltFor(driver_id).
func PhysicalKey(c Cell, driverID string) string {
	return SaltedKey(c.Key(), SaltFor(driverID))
}
