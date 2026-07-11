package sharding

import "fmt"

// RemapReport records the outcome of moving one logical shard, phase by phase.
// The remap-under-load test asserts WriteErrors == 0 and Misroutes == 0.
type RemapReport struct {
	Shard      int
	From       string
	To         string
	Copied     int  // rows backfilled (copy-if-absent) from old → new owner
	DualWrites int  // observed dual-writes during the window (informational)
	Verified   int  // rows compared equal at verify
	CleanedUp  int  // rows dropped from the old owner after cutover
	WriteErrors int // writes that returned an error during the move
	Misroutes  int  // keys not readable at their expected value after cutover
}

func (r RemapReport) String() string {
	return fmt.Sprintf("remap shard %d %s→%s: copied=%d dual=%d verified=%d cleaned=%d write_errors=%d misroutes=%d",
		r.Shard, r.From, r.To, r.Copied, r.DualWrites, r.Verified, r.CleanedUp, r.WriteErrors, r.Misroutes)
}

// RemapHooks lets a driver (test or CLI) observe phase boundaries — e.g. to
// spin up write load right as the dual-write window opens, or to pause between
// phases in a runbook. All hooks are optional.
type RemapHooks struct {
	OnDualWriteStart func()          // dual-write window is now open
	AfterBackfill    func(copied int) // copy-if-absent finished
	BeforeCutover    func()          // about to freeze the shard + flip
	AfterCutover     func()          // table flipped to the new owner
}

// Move relocates one logical shard from its current owner to target `to` online,
// with the D6 / V-T26 sequence:
//
//	1. copy      — open the dual-write window, then backfill existing rows
//	               copy-if-absent so a concurrent dual-write is never clobbered.
//	2. dual-write— (the window stays open across steps 1-3) every new write to
//	               the shard lands on BOTH owners atomically.
//	3. verify    — freeze the shard (mu.Lock) and assert old[shard] == new[shard].
//	4. cutover   — while still frozen, flip the table to `to` and close the
//	               window; writes/reads now go to `to` only. Then clean up old.
//
// Zero misroutes / zero write errors during the move is a property of this
// ordering plus the Cluster concurrency model (see cluster.go). Move is NOT safe
// to call concurrently with another Move on the same cluster.
func (c *Cluster) Move(shard int, to string, hooks *RemapHooks) (RemapReport, error) {
	rep := RemapReport{Shard: shard, To: to}
	if shard < 0 || shard >= NumLogicalShards {
		return rep, fmt.Errorf("sharding: shard %d out of range", shard)
	}
	if _, ok := c.stores[to]; !ok {
		return rep, fmt.Errorf("sharding: unknown target %q", to)
	}

	// Determine current owner and open the dual-write window (exclusive, so no
	// single-write to this shard is still in flight afterwards).
	c.mu.Lock()
	from := c.table[shard]
	rep.From = from
	if from == to {
		c.mu.Unlock()
		return rep, fmt.Errorf("sharding: shard %d already on %q", shard, to)
	}
	c.move = &moveState{shard: shard, from: from, to: to}
	c.mu.Unlock()
	dualStart := c.dualWrites.Load()
	if hooks != nil && hooks.OnDualWriteStart != nil {
		hooks.OnDualWriteStart()
	}

	// Phase 1: backfill. Snapshot is taken AFTER the window opened, so any row
	// written single-owner before the window is included; anything written after
	// is already dual-written to `to` and skipped by copy-if-absent.
	src := c.stores[from].SnapshotShard(shard)
	dst := c.stores[to]
	for k, v := range src {
		if dst.PutIfAbsent(shard, k, v) {
			rep.Copied++
		}
	}
	if hooks != nil && hooks.AfterBackfill != nil {
		hooks.AfterBackfill(rep.Copied)
	}

	// Phase 3: freeze the shard, verify, then cut over atomically.
	if hooks != nil && hooks.BeforeCutover != nil {
		hooks.BeforeCutover()
	}
	c.mu.Lock()
	// Under mu.Lock all in-flight dual-writes have drained, so this equality is
	// exact, not a racy snapshot.
	a := c.stores[from].SnapshotShard(shard)
	b := c.stores[to].SnapshotShard(shard)
	if len(a) != len(b) {
		c.mu.Unlock()
		return rep, fmt.Errorf("sharding: verify failed: %s has %d rows, %s has %d for shard %d",
			from, len(a), to, len(b), shard)
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || bv != av {
			c.mu.Unlock()
			return rep, fmt.Errorf("sharding: verify mismatch on key %q shard %d", k, shard)
		}
	}
	rep.Verified = len(b)
	// Cutover: flip the table and close the window in the same critical section.
	c.table[shard] = to
	c.move = nil
	c.mu.Unlock()
	if hooks != nil && hooks.AfterCutover != nil {
		hooks.AfterCutover()
	}

	// Phase 4: drop the shard from the old owner (reads/writes no longer touch it).
	rep.CleanedUp = c.stores[from].DeleteShard(shard)
	rep.DualWrites = int(c.dualWrites.Load() - dualStart)
	return rep, nil
}
