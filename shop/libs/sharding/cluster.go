package sharding

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Cluster is the sandbox reference integration: it routes entity keys across N
// fake physical Stores using the logical-shard table, and it hosts the online
// remap (copy → dual-write → verify → cutover). It is the end-to-end harness the
// task's DoD ("sandbox routes end-to-end") and remap-under-load test drive.
//
// # Concurrency model (why the remap is misroute-free)
//
// Every Put/Get takes mu.RLock for the whole operation and reads the routing
// state under it; a phase transition (enter dual-write, cutover) takes mu.Lock.
// Because mu.Lock waits for all in-flight RLock holders, no writer can still be
// mid-single-write when dual-write begins, and none can be mid-dual-write when
// cutover flips the table. Reads run concurrently (RLock is shared); only the
// two brief transitions are exclusive.
//
// The moving shard's dual-write pairs the two Store puts under dualMu so two
// concurrent writers to the same key can never leave A and B disagreeing. That
// makes the pre-cutover verify (A[shard] == B[shard]) a real equality, not a
// racy snapshot.
type Cluster struct {
	stores map[string]*Store

	mu     sync.RWMutex // guards the routing state below
	table  [NumLogicalShards]string
	move   *moveState // non-nil only during a remap

	dualMu sync.Mutex // pairs the two puts of a moving-shard dual-write

	dualWrites atomic.Uint64 // count of dual-writes ever performed (informational)
}

type moveState struct {
	shard int
	from  string
	to    string
}

// NewCluster builds a sandbox from a routing table and its backing stores. The
// table's target names must all exist in stores.
func NewCluster(table [NumLogicalShards]string, stores map[string]*Store) (*Cluster, error) {
	for s, t := range table {
		if _, ok := stores[t]; !ok {
			return nil, fmt.Errorf("sharding: shard %d routes to unknown target %q", s, t)
		}
	}
	c := &Cluster{stores: stores, table: table}
	return c, nil
}

// NewClusterFromConfig builds a sandbox from a Config, creating one empty Store
// per declared target.
func NewClusterFromConfig(cfg *Config) (*Cluster, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tab, err := cfg.Table()
	if err != nil {
		return nil, err
	}
	stores := map[string]*Store{}
	for name := range cfg.Targets {
		stores[name] = NewStore(name)
	}
	return NewCluster(tab, stores)
}

// Put routes key to its physical target and writes value. During a remap of the
// key's shard it dual-writes to both the old and new target atomically. Returns
// the physical target the write landed on (authoritative for reads).
func (c *Cluster) Put(key, value string) (string, error) {
	shard := LogicalShard(key)

	// Hold RLock across the ENTIRE operation so the routing decision and the
	// store write are atomic with respect to a Move's exclusive transitions
	// (enter dual-write / cutover, which take mu.Lock and thus wait for every
	// in-flight RLock holder). Releasing the lock before the write would let a
	// cutover+cleanup slip in and drop the write — a misroute.
	c.mu.RLock()
	defer c.mu.RUnlock()

	if m := c.move; m != nil && m.shard == shard {
		// dual-write window: write both owners, paired under dualMu so two
		// concurrent writers can never leave `from` and `to` disagreeing.
		c.dualMu.Lock()
		c.stores[m.from].Put(shard, key, value)
		c.stores[m.to].Put(shard, key, value)
		c.dualMu.Unlock()
		c.dualWrites.Add(1)
		return m.to, nil
	}
	target := c.table[shard]
	st, ok := c.stores[target]
	if !ok {
		return "", fmt.Errorf("sharding: shard %d routes to unknown target %q", shard, target)
	}
	st.Put(shard, key, value)
	return target, nil
}

// Get reads key from its currently authoritative target. During a remap reads
// stay on the old owner (which is always complete) until cutover flips the
// table; after cutover they read the new owner.
func (c *Cluster) Get(key string) (string, bool) {
	shard := LogicalShard(key)
	// RLock spans the read so the routing decision and the store read are atomic
	// with cutover+cleanup (which would otherwise delete the row from `from`
	// between our table read and our store read).
	c.mu.RLock()
	defer c.mu.RUnlock()
	if m := c.move; m != nil && m.shard == shard {
		// during the window the old owner is always complete; read it.
		return c.stores[m.from].Get(key)
	}
	st, ok := c.stores[c.table[shard]]
	if !ok {
		return "", false
	}
	return st.Get(key)
}

// Physical returns the current authoritative target for a shard.
func (c *Cluster) Physical(shard int) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.table[shard]
}

// Store returns a named store (test/inspection helper).
func (c *Cluster) Store(name string) *Store { return c.stores[name] }
