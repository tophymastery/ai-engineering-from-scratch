package main

import "github.com/shop-platform/shop/libs/sharding"

// sharding.go — D11: the merchant incoming-order read model is SHARDED BY
// merchant_id. Every queue row is placed by the deterministic routing primitive
// from libs/sharding: LogicalShard(merchant_id) ∈ [0,256) → a physical cell via
// a fixed shard→cell map. The same merchant_id always lands in the same
// logical shard and cell on every process (the routing math is pure, std-lib
// only), which is exactly what lets the rebuild tool target ONE cell and lets a
// merchant's whole queue live on one physical partition (no cross-shard fan-out
// for a merchant's queue read).

// NumCells is the physical cell count for the merchant-queue store (D11; start
// small, split by remapping like the D6 4→8 drill). Powers-of-two so the shard→
// cell reduction is uniform (LogicalShard is already avalanched, murmur3 fmix64).
const NumCells = 4

// logicalShard returns the 256-way logical shard for a merchant_id (D11 / D6
// routing primitive). Deterministic and pure.
func logicalShard(merchantID string) int {
	return sharding.LogicalShard(merchantID)
}

// cellFor maps a merchant_id to its physical cell in [0, NumCells). The map is
// shard % NumCells — deterministic, and because LogicalShard has already
// avalanched the key, the cell distribution is uniform.
func cellFor(merchantID string) int {
	return sharding.LogicalShard(merchantID) % NumCells
}
