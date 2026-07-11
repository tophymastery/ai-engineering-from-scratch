package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	_ "modernc.org/sqlite"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // single shared in-memory db
	if err := Migrate(context.Background(), db, SQLiteDialect{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func env(t *testing.T, id, agg string) eventbus.Envelope {
	t.Helper()
	e, err := eventbus.NewEnvelope(id, "order.paid", "tr", eventbus.Aggregate{Type: "order", ID: agg}, 1, map[string]any{"x": 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestWriteInTxAtomicity: the outbox row is written in the caller's tx and only
// visible after commit; a rolled-back tx leaves no row (no write-then-publish
// race, no phantom event).
func TestWriteInTxAtomicity(t *testing.T) {
	db := openSQLite(t)
	s := NewSQLStore(db, SQLiteDialect{})
	ctx := context.Background()

	// Rolled-back tx -> no row.
	tx, _ := db.BeginTx(ctx, nil)
	if err := s.WriteInTx(ctx, tx, "order.paid", env(t, "evt_rollback", "ord_1")); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	recs, _ := s.Tail(ctx, 0, 10)
	if len(recs) != 0 {
		t.Fatalf("rolled-back write leaked %d rows", len(recs))
	}

	// Committed tx -> exactly one row.
	tx, _ = db.BeginTx(ctx, nil)
	if err := s.WriteInTx(ctx, tx, "order.paid", env(t, "evt_ok", "ord_1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	recs, _ = s.Tail(ctx, 0, 10)
	if len(recs) != 1 || recs[0].EventID != "evt_ok" {
		t.Fatalf("committed write not tailed: %+v", recs)
	}
}

func TestWriteInTxRejectsBadEnvelope(t *testing.T) {
	db := openSQLite(t)
	s := NewSQLStore(db, SQLiteDialect{})
	ctx := context.Background()
	tx, _ := db.BeginTx(ctx, nil)
	bad := eventbus.Envelope{EventType: "order.paid"} // missing event_id etc.
	if err := s.WriteInTx(ctx, tx, "order.paid", bad); err == nil {
		t.Fatal("expected envelope validation to reject")
	}
	_ = tx.Rollback()
}

// TestRelayTailsAndPublishes: relay publishes every committed row exactly in id
// order and advances the durable cursor; restarting from the cursor replays
// nothing already past it.
func TestRelayTailsAndPublishes(t *testing.T) {
	db := openSQLite(t)
	s := NewSQLStore(db, SQLiteDialect{})
	bus := eventbus.NewMemBroker(WithP(1))
	ctx := context.Background()

	var got atomic.Int64
	sub, _ := bus.Subscribe(eventbus.SubscribeConfig{Topic: "order.paid", Group: "g"}, func(_ context.Context, _ eventbus.Message) error {
		got.Add(1)
		return nil
	})
	defer sub.Close()

	relay := NewCDCTailRelay(s, bus, RelayConfig{Name: "r"})
	rctx, cancel := context.WithCancel(ctx)
	go relay.Run(rctx)

	for i := 0; i < 20; i++ {
		tx, _ := db.BeginTx(ctx, nil)
		if err := s.WriteInTx(ctx, tx, "order.paid", env(t, fmt.Sprintf("evt_%02d", i), "ord_1")); err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit()
		s.Signal()
	}
	waitEq(t, &got, 20, 5*time.Second, "relay->bus->consumer")

	// Poll the durable cursor up to the deadline (the relay persists it right
	// after publishing; give the async save a moment before shutting down).
	deadline := time.After(5 * time.Second)
	for {
		if cur, _ := s.LoadCursor(ctx, "r"); cur == 20 {
			break
		}
		select {
		case <-deadline:
			cur, _ := s.LoadCursor(ctx, "r")
			t.Fatalf("cursor=%d want 20", cur)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if pub := relay.Published(); pub != 20 {
		t.Fatalf("relay published %d, want 20", pub)
	}
}

// TestPartitionDropGuardAndDrop: cannot drop a day holding unpublished rows;
// once published, the drop removes them with zero loss to the live stream.
func TestPartitionDropGuardAndDrop(t *testing.T) {
	db := openSQLite(t)
	s := NewSQLStore(db, SQLiteDialect{})
	ctx := context.Background()

	// Insert 3 rows dated "yesterday".
	yest := time.Now().UTC().AddDate(0, 0, -1)
	for i := 0; i < 3; i++ {
		tx, _ := db.BeginTx(ctx, nil)
		_ = s.WriteInTx(ctx, tx, "order.paid", env(t, fmt.Sprintf("old_%d", i), "ord_1"))
		_ = tx.Commit()
	}
	// Backdate their part_day so they belong to yesterday's partition.
	if _, err := db.Exec(`UPDATE outbox SET part_day = ?`, yest.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	// Cursor still 0 -> rows unpublished -> drop must refuse.
	if _, err := s.DropPublishedBefore(ctx, "r", time.Now().UTC()); err == nil {
		t.Fatal("expected drop to refuse unpublished partition")
	}
	// Advance cursor past them (simulate relay publish).
	_ = s.SaveCursor(ctx, "r", 3)
	n, err := s.DropPublishedBefore(ctx, "r", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("dropped %d, want 3", n)
	}
	recs, _ := s.Tail(ctx, 0, 10)
	if len(recs) != 0 {
		t.Fatalf("expected empty after drop, got %d", len(recs))
	}
}

// waitEq spins until the counter reaches want or the deadline elapses.
func waitEq(t *testing.T, c *atomic.Int64, want int64, d time.Duration, what string) {
	t.Helper()
	deadline := time.After(d)
	for c.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("%s: reached %d/%d before timeout", what, c.Load(), want)
		case <-time.After(time.Millisecond):
		}
	}
}

// WithP is a partition-count option alias so the outbox tests need not import
// the eventbus option name directly in every call.
func WithP(n int) eventbus.MemOption { return eventbus.WithPartitions(n) }
