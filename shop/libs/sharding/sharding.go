// Package sharding implements D6: application-level sharding over plain
// PostgreSQL. A cell has a fixed number of LOGICAL shards (256) mapped, via a
// config-driven and hot-reloadable table, to N PHYSICAL clusters (start N=4;
// split by remapping). An entity key (customer_id, order_id, user_id …) hashes
// deterministically to a logical shard; the logical shard routes to a physical
// target. Prefixed ULIDs carry a 2-hex-char shard hint after the prefix so a
// point lookup recovers its shard with zero directory reads.
//
// The routing math is deterministic and dependency-light (std lib only): the
// same key always maps to the same logical shard on every process, in every
// language that reimplements the documented hash. That determinism is what lets
// the shard hint be embedded in an ID at creation time and trusted forever.
//
// # Hash
//
// LogicalShard(key) = fmix64(fnv1a64(key)) mod NumLogicalShards.
//
// FNV-1a gives a fast, allocation-light 64-bit digest; fnv1a64's low bits are
// weakly mixed, and NumLogicalShards is a power of two, so a bare `% 256` would
// read only those weak low bits. We therefore pass the digest through the
// murmur3 fmix64 finalizer (two multiply/xor-shift rounds) which avalanches all
// 64 bits into the low 8, yielding a near-perfectly uniform shard (proven by the
// 1M-key chi-square test in this package). The finalizer constants are the
// public murmur3 ones and are part of the wire contract: any reimplementation
// MUST reproduce them to stay route-compatible.
package sharding

import "hash/fnv"

// NumLogicalShards is the fixed logical-shard count per cell (D6). It is a
// power of two so a shard fits in exactly 2 hex characters (00..ff), which is
// the ULID shard hint. Changing it is a cell-wide re-key and is out of scope.
const NumLogicalShards = 256

// Hash64 returns the finalized 64-bit digest of key. Exposed so callers (and
// cross-language reimplementations) can verify they compute the identical value
// that LogicalShard reduces.
func Hash64(key string) uint64 {
	h := fnv.New64a()
	// fnv.Write never returns an error and never allocates for a string via the
	// []byte conversion in a hot path the compiler keeps stack-local.
	_, _ = h.Write([]byte(key))
	return fmix64(h.Sum64())
}

// LogicalShard maps an entity key to its logical shard in [0, NumLogicalShards).
// Deterministic and pure: identical on every process and safe for concurrent
// use. This is THE routing primitive — the ULID shard hint and the physical
// router both derive from it.
func LogicalShard(key string) int {
	return int(Hash64(key) % NumLogicalShards)
}

// fmix64 is the murmur3 64-bit finalizer: it avalanches the whole word into the
// low bits so a power-of-two modulus is uniform. Do NOT change the constants —
// they are part of the cross-language routing contract.
func fmix64(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}
