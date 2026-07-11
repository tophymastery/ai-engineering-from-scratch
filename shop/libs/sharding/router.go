package sharding

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Router resolves a logical shard (or an entity key, or a shard-hint ID) to its
// physical target using a config-driven table. It is hot-reloadable: Reload
// re-reads the file and swaps the table atomically, and Watch polls the file's
// mtime so an operator editing the map on disk takes effect without a restart.
//
// Reads are lock-free (an atomic table snapshot) so the hot path — millions of
// route lookups per second — never contends with a rare reload. Safe for
// concurrent use.
type Router struct {
	path string
	tab  atomic.Pointer[routeTable]

	watchOnce sync.Once
	stop      chan struct{}
}

type routeTable struct {
	version int
	table   [NumLogicalShards]string
	targets map[string]string // name -> DSN/label (for connection lookup)
	mtime   time.Time
}

// NewRouter builds a Router directly from an in-memory Config (no file). Useful
// for tests and for callers that assemble the map themselves.
func NewRouter(cfg *Config) (*Router, error) {
	rt, err := buildTable(cfg, time.Time{})
	if err != nil {
		return nil, err
	}
	r := &Router{stop: make(chan struct{})}
	r.tab.Store(rt)
	return r, nil
}

// OpenRouter loads the routing map from path and returns a Router bound to it,
// so a later Reload()/Watch() re-reads the same file.
func OpenRouter(path string) (*Router, error) {
	r := &Router{path: path, stop: make(chan struct{})}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func buildTable(cfg *Config, mtime time.Time) (*routeTable, error) {
	tab, err := cfg.Table()
	if err != nil {
		return nil, err
	}
	targets := make(map[string]string, len(cfg.Targets))
	for k, v := range cfg.Targets {
		targets[k] = v
	}
	return &routeTable{version: cfg.Version, table: tab, targets: targets, mtime: mtime}, nil
}

// Reload re-reads the bound file and atomically swaps in the new table. On any
// error (missing file, bad shape, incomplete coverage) the current table is
// left untouched — a broken edit never black-holes live routing.
func (r *Router) Reload() error {
	if r.path == "" {
		return fmt.Errorf("sharding: router has no bound file to reload")
	}
	fi, err := os.Stat(r.path)
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(r.path)
	if err != nil {
		return err
	}
	rt, err := buildTable(cfg, fi.ModTime())
	if err != nil {
		return err
	}
	r.tab.Store(rt)
	return nil
}

// Physical returns the physical target name owning a logical shard.
func (r *Router) Physical(shard int) string {
	return r.tab.Load().table[shard]
}

// Target returns the target name and its DSN/label for a logical shard.
func (r *Router) Target(shard int) (name, dsn string) {
	rt := r.tab.Load()
	name = rt.table[shard]
	return name, rt.targets[name]
}

// RouteKey resolves an entity key straight to its physical target (hash → shard
// → target) in one call.
func (r *Router) RouteKey(key string) (shard int, target string) {
	shard = LogicalShard(key)
	return shard, r.tab.Load().table[shard]
}

// RouteID resolves a shard-hint ULID to its physical target using ONLY the
// embedded hint — no hashing, no key needed. This is the point-lookup fast path.
func (r *Router) RouteID(id string) (shard int, target string, err error) {
	shard, err = DecodeShard(id)
	if err != nil {
		return 0, "", err
	}
	return shard, r.tab.Load().table[shard], nil
}

// Version reports the loaded config version (for observability / drift checks).
func (r *Router) Version() int { return r.tab.Load().version }

// Targets returns a copy of the declared target→DSN map.
func (r *Router) Targets() map[string]string {
	rt := r.tab.Load()
	out := make(map[string]string, len(rt.targets))
	for k, v := range rt.targets {
		out[k] = v
	}
	return out
}

// Watch starts a background goroutine that polls the bound file's mtime every
// interval and Reload()s when it changes. Errors from a reload are delivered to
// onErr (nil to ignore) so a bad edit is observable but non-fatal. Idempotent:
// only the first call starts the watcher. Stop() ends it.
func (r *Router) Watch(interval time.Duration, onErr func(error)) {
	r.watchOnce.Do(func() {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-r.stop:
					return
				case <-t.C:
					fi, err := os.Stat(r.path)
					if err != nil {
						if onErr != nil {
							onErr(err)
						}
						continue
					}
					if fi.ModTime().Equal(r.tab.Load().mtime) {
						continue
					}
					if err := r.Reload(); err != nil && onErr != nil {
						onErr(err)
					}
				}
			}
		}()
	})
}

// Stop ends the Watch goroutine (safe to call even if Watch never ran, but not
// more than once).
func (r *Router) Stop() { close(r.stop) }
