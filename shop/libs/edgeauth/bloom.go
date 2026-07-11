package edgeauth

import (
	"encoding/base64"
	"encoding/binary"
	"crypto/sha256"
	"fmt"
	"sync"
)

// std base64 (with padding) for the bitset blob — it is opaque bytes, not a JWT
// segment, so the conventional alphabet is fine and keeps the snapshot compact.
var stdb64 = base64.StdEncoding

// Bloom is the replicated revocation denylist (D4). Adding a jti can never be
// undone within a snapshot, and membership has NO false negatives — a revoked
// token is ALWAYS caught (the safety property). False positives (rejecting a
// still-valid token) are possible but negligibly rare at the sizes used here,
// and are the acceptable trade the decision names. Safe for concurrent Add/Test.
type Bloom struct {
	mu    sync.RWMutex
	m     uint32 // number of bits (rounded to a multiple of 8)
	k     uint32 // number of hash probes
	bits  []byte
	count uint64 // jtis added (for observability; not authoritative for FP rate)
}

// NewBloom builds an empty filter with m bits and k probes. m is rounded up to a
// whole byte. Defaults (0,0) → 1<<16 bits (8 KiB) and k=7, which keeps the false
// positive rate < 1e-4 for tens of thousands of live revocations.
func NewBloom(m, k uint32) *Bloom {
	if m == 0 {
		m = 1 << 16
	}
	if k == 0 {
		k = 7
	}
	m = (m + 7) &^ 7 // whole bytes
	return &Bloom{m: m, k: k, bits: make([]byte, m/8)}
}

// probes derives k bit-indices from two 32-bit halves of SHA-256(jti) via
// Kirsch-Mitzenmacher double hashing: idx_i = (h1 + i*h2) mod m. Deterministic
// and identical on issuer and verifier — that parity is why this lives in the
// shared lib.
func (b *Bloom) probes(jti string) []uint32 {
	sum := sha256.Sum256([]byte(jti))
	h1 := binary.BigEndian.Uint32(sum[0:4])
	h2 := binary.BigEndian.Uint32(sum[4:8])
	if h2 == 0 {
		h2 = 1
	}
	out := make([]uint32, b.k)
	for i := uint32(0); i < b.k; i++ {
		out[i] = (h1 + i*h2) % b.m
	}
	return out
}

// Add records a revoked jti.
func (b *Bloom) Add(jti string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, idx := range b.probes(jti) {
		b.bits[idx/8] |= 1 << (idx % 8)
	}
	b.count++
}

// Test reports whether jti MIGHT be revoked. false ⇒ definitely not revoked.
func (b *Bloom) Test(jti string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, idx := range b.probes(jti) {
		if b.bits[idx/8]&(1<<(idx%8)) == 0 {
			return false
		}
	}
	return true
}

// Count returns how many jtis have been added.
func (b *Bloom) Count() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// BloomSnapshot is the wire form the gateway polls (GET /v1/auth/denylist). The
// version increments on every Add so a poller can cheaply detect change; k/m let
// the verifier reconstruct an identical filter; bits is the base64 bitset.
type BloomSnapshot struct {
	Version uint64 `json:"version"`
	K       uint32 `json:"k"`
	M       uint32 `json:"m"`
	Count   uint64 `json:"count"`
	Bits    string `json:"bits"` // base64(std) of the m/8-byte bitset
}

// Snapshot renders the current filter at the given monotonic version.
func (b *Bloom) Snapshot(version uint64) BloomSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cp := make([]byte, len(b.bits))
	copy(cp, b.bits)
	return BloomSnapshot{
		Version: version,
		K:       b.k,
		M:       b.m,
		Count:   b.count,
		Bits:    stdb64.EncodeToString(cp),
	}
}

// BloomFromSnapshot reconstructs a filter from a polled snapshot. The verifier
// calls this; Test on the result is bit-identical to Test on the issuer's live
// filter for the same version.
func BloomFromSnapshot(s BloomSnapshot) (*Bloom, error) {
	if s.M == 0 || s.K == 0 {
		return nil, fmt.Errorf("edgeauth: snapshot missing k/m")
	}
	bits, err := stdb64.DecodeString(s.Bits)
	if err != nil {
		return nil, fmt.Errorf("edgeauth: snapshot bits: %w", err)
	}
	if uint32(len(bits)) != s.M/8 {
		return nil, fmt.Errorf("edgeauth: snapshot bits length %d != m/8 %d", len(bits), s.M/8)
	}
	return &Bloom{m: s.M, k: s.K, bits: bits, count: s.Count}, nil
}
