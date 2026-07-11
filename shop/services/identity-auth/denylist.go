package main

import (
	"sync"
	"sync/atomic"

	"github.com/shop-platform/shop/libs/edgeauth"
)

// denylist is the service's authoritative revocation set (D4): an in-memory
// bloom filter plus a monotonic version. Revoke() adds a jti and bumps the
// version; the gateway polls Snapshot() ≤30 s and reconstructs an identical
// filter. In a multi-cell deployment this is replicated (the snapshot is the
// replication unit); here one process is the source of truth.
type denylist struct {
	mu      sync.Mutex
	bloom   *edgeauth.Bloom
	version atomic.Uint64
}

func newDenylist() *denylist {
	return &denylist{bloom: edgeauth.NewBloom(0, 0)}
}

// revoke adds a jti and returns the new version.
func (d *denylist) revoke(jti string) uint64 {
	d.mu.Lock()
	d.bloom.Add(jti)
	d.mu.Unlock()
	return d.version.Add(1)
}

// snapshot renders the current denylist for the poll endpoint.
func (d *denylist) snapshot() edgeauth.BloomSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bloom.Snapshot(d.version.Load())
}
