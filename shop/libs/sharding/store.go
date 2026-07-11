package sharding

import "sync"

// Store is one fake physical target for the sandbox reference integration
// (scope item 4). It is an in-memory, concurrency-safe key/value map standing in
// for a physical PostgreSQL cluster — no I/O, so the 1M-scale and under-load
// tests stay in-memory and fast (the NOTE in the task). Each stored row records
// the logical shard it belongs to so the remap can snapshot a single shard and
// so misroute checks can inspect residency.
type Store struct {
	name string
	mu   sync.RWMutex
	data map[string]row
}

type row struct {
	shard int
	value string
}

// NewStore creates an empty named fake physical target.
func NewStore(name string) *Store {
	return &Store{name: name, data: map[string]row{}}
}

// Name returns the target name.
func (s *Store) Name() string { return s.name }

// Put upserts key→value tagged with its logical shard (last write wins).
func (s *Store) Put(shard int, key, value string) {
	s.mu.Lock()
	s.data[key] = row{shard: shard, value: value}
	s.mu.Unlock()
}

// PutIfAbsent inserts only if key is not already present; reports whether it
// inserted. This is the backfill-copy primitive: a concurrent dual-write that
// already placed a newer value is never clobbered by the copy.
func (s *Store) PutIfAbsent(shard int, key, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		return false
	}
	s.data[key] = row{shard: shard, value: value}
	return true
}

// Get returns the value and whether it is present.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	r, ok := s.data[key]
	s.mu.RUnlock()
	return r.value, ok
}

// SnapshotShard returns a copy of every key/value currently owned by shard.
func (s *Store) SnapshotShard(shard int) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for k, r := range s.data {
		if r.shard == shard {
			out[k] = r.value
		}
	}
	return out
}

// DeleteShard drops all rows for shard (post-cutover cleanup on the old owner).
func (s *Store) DeleteShard(shard int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, r := range s.data {
		if r.shard == shard {
			delete(s.data, k)
			n++
		}
	}
	return n
}

// Len reports the row count (test/inspection helper).
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
