package sharding

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigJSONAndYAMLAgree(t *testing.T) {
	jc, err := LoadConfig("testdata/routing.4x256.json")
	if err != nil {
		t.Fatal(err)
	}
	yc, err := LoadConfig("testdata/routing.4x256.yaml")
	if err != nil {
		t.Fatal(err)
	}
	jt, err := jc.Table()
	if err != nil {
		t.Fatal(err)
	}
	yt, err := yc.Table()
	if err != nil {
		t.Fatal(err)
	}
	if jt != yt {
		t.Fatal("JSON and YAML routing tables differ")
	}
	// spot-check assignment boundaries
	if jt[0] != "pg-0" || jt[63] != "pg-0" || jt[64] != "pg-1" || jt[255] != "pg-3" {
		t.Fatalf("unexpected boundaries: %q %q %q %q", jt[0], jt[63], jt[64], jt[255])
	}
}

func TestConfigValidateRejectsGaps(t *testing.T) {
	bad := &Config{
		Targets:     map[string]string{"pg-0": "x"},
		Assignments: []Assignment{{Target: "pg-0", Shards: "0-100"}}, // 101..255 missing
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for uncovered shards")
	}
	overlap := &Config{
		Targets: map[string]string{"pg-0": "x", "pg-1": "y"},
		Assignments: []Assignment{
			{Target: "pg-0", Shards: "0-255"},
			{Target: "pg-1", Shards: "128"}, // overlaps
		},
	}
	if err := overlap.Validate(); err == nil {
		t.Fatal("expected validation error for overlapping shards")
	}
	unknown := &Config{
		Targets:     map[string]string{"pg-0": "x"},
		Assignments: []Assignment{{Target: "pg-9", Shards: "0-255"}},
	}
	if err := unknown.Validate(); err == nil {
		t.Fatal("expected validation error for unknown target")
	}
}

func TestRouterRouteKeyAndID(t *testing.T) {
	r, err := OpenRouter("testdata/routing.4x256.json")
	if err != nil {
		t.Fatal(err)
	}
	key := "cus_01HTESTCUSTOMER"
	shard, target := r.RouteKey(key)
	if shard != LogicalShard(key) {
		t.Fatalf("RouteKey shard %d != LogicalShard %d", shard, LogicalShard(key))
	}
	if target == "" {
		t.Fatal("empty target")
	}
	// An ID minted for the same key must route to the same target via hint only.
	id := NewID("ord", key)
	sID, tID, err := r.RouteID(id)
	if err != nil {
		t.Fatal(err)
	}
	if sID != shard || tID != target {
		t.Fatalf("RouteID (%d,%s) != RouteKey (%d,%s)", sID, tID, shard, target)
	}
}

// TestRouterHotReload proves an on-disk edit (a 4→8 physical split) is picked up
// by Reload() and by the mtime-polling Watch() without a restart.
func TestRouterHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.json")
	copyFile(t, "testdata/routing.4x256.json", path)

	r, err := OpenRouter(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version() != 1 {
		t.Fatalf("initial version = %d, want 1", r.Version())
	}
	if got := r.Physical(255); got != "pg-3" {
		t.Fatalf("shard 255 initially on %q, want pg-3", got)
	}

	// Explicit Reload path.
	copyFile(t, "testdata/routing.8x256.json", path)
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if r.Version() != 2 {
		t.Fatalf("after reload version = %d, want 2", r.Version())
	}
	if got := r.Physical(255); got != "pg-7" {
		t.Fatalf("shard 255 after 4→8 reload on %q, want pg-7", got)
	}
	if got := r.Physical(0); got != "pg-0" {
		t.Fatalf("shard 0 after reload on %q, want pg-0", got)
	}

	// Watch path: revert on disk, expect the poller to pick it up.
	errc := make(chan error, 8)
	r.Watch(10*time.Millisecond, func(e error) { errc <- e })
	// ensure a distinct mtime
	time.Sleep(15 * time.Millisecond)
	copyFile(t, "testdata/routing.4x256.json", path)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Second))

	deadline := time.After(3 * time.Second)
	for {
		if r.Version() == 1 && r.Physical(255) == "pg-3" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("watch did not reload within 3s (version=%d)", r.Version())
		case e := <-errc:
			t.Logf("watch reload error (non-fatal): %v", e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	r.Stop()
}

// TestReloadIgnoresBrokenEdit: a malformed edit must not black-hole live routing.
func TestReloadIgnoresBrokenEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.json")
	copyFile(t, "testdata/routing.4x256.json", path)
	r, err := OpenRouter(path)
	if err != nil {
		t.Fatal(err)
	}
	before := r.Physical(100)
	if err := os.WriteFile(path, []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(); err == nil {
		t.Fatal("expected reload error on malformed file")
	}
	if after := r.Physical(100); after != before {
		t.Fatalf("broken edit changed live routing: %q -> %q", before, after)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
