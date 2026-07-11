package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shop-platform/shop/libs/inbox"
	"github.com/shop-platform/shop/libs/outbox"
	_ "modernc.org/sqlite"
)

// TestDLQCtlDemo drives the full CLI: seed -> list -> inspect -> replay, and
// asserts the parked event is marked replayed AND re-emitted into the outbox
// (the durable handoff that lets the relay reprocess it exactly-once).
func TestDLQCtlDemo(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "dlq.db")

	mustRun := func(want int, args ...string) string {
		var buf bytes.Buffer
		full := append([]string{"-db", dbFile}, args...)
		if code := run(full, &buf); code != want {
			t.Fatalf("dlqctl %v => exit %d, want %d\n%s", args, code, want, buf.String())
		}
		return buf.String()
	}

	// seed
	if out := mustRun(0, "seed"); !strings.Contains(out, "seeded 2 parked") {
		t.Fatalf("seed output: %s", out)
	}
	// depth == 2
	if out := mustRun(0, "depth", "-group", "projection"); strings.TrimSpace(out) != "2" {
		t.Fatalf("depth: %q", out)
	}
	// list shows both parked
	list := mustRun(0, "list", "-group", "projection", "-status", "parked")
	if !strings.Contains(list, "evt_seed_0") || !strings.Contains(list, "evt_seed_1") || !strings.Contains(list, "(2 rows)") {
		t.Fatalf("list: %s", list)
	}
	// inspect id=1
	insp := mustRun(0, "inspect", "1")
	for _, want := range []string{"event_id:   evt_seed_0", "attempts:   3", "status:     parked", "envelope:"} {
		if !strings.Contains(insp, want) {
			t.Fatalf("inspect missing %q:\n%s", want, insp)
		}
	}
	// replay id=1
	rep := mustRun(0, "replay", "1")
	if !strings.Contains(rep, "replayed id=1") || !strings.Contains(rep, "replayed 1 row") {
		t.Fatalf("replay: %s", rep)
	}

	// Verify durable effects: DLQ depth dropped to 1, and the outbox now holds
	// the re-emitted event ready for the relay.
	db, err := sql.Open("sqlite", "file:"+dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dlq := inbox.NewSQLDLQ(db, inbox.SQLiteDialect{})
	if d, _ := dlq.Depth(context.Background(), "projection"); d != 1 {
		t.Fatalf("depth after replay=%d want 1", d)
	}
	store := outbox.NewSQLStore(db, outbox.SQLiteDialect{})
	recs, err := store.Tail(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].EventID != "evt_seed_0" {
		t.Fatalf("replay did not re-emit into outbox: %+v", recs)
	}

	// replay -group -all replays the remaining parked row.
	all := mustRun(0, "replay", "-group", "projection", "-all")
	if !strings.Contains(all, "replayed 1 row") {
		t.Fatalf("replay -all: %s", all)
	}
	if d, _ := dlq.Depth(context.Background(), "projection"); d != 0 {
		t.Fatalf("depth after replay-all=%d want 0", d)
	}
}
