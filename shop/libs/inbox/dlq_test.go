package inbox

import (
	"context"
	"fmt"
	"testing"

	"github.com/shop-platform/shop/libs/eventbus"
)

func TestDLQParkListInspectReplay(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	dlq := NewSQLDLQ(db, SQLiteDialect{})

	// Park two messages.
	for i := 0; i < 2; i++ {
		if err := dlq.Park(ctx, msg(t, fmt.Sprintf("evt_%d", i)), "g", 3, fmt.Errorf("permanent failure %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// List.
	rows, err := dlq.List(ctx, "g", StatusParked)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: rows=%d err=%v", len(rows), err)
	}
	if d, _ := dlq.Depth(ctx, "g"); d != 2 {
		t.Fatalf("depth=%d want 2", d)
	}
	// Inspect one.
	got, err := dlq.Get(ctx, rows[0].ID)
	if err != nil || got.EventID != "evt_0" || got.Attempts != 3 {
		t.Fatalf("inspect: %+v err=%v", got, err)
	}

	// Replay one — republisher captures the reconstructed message.
	var replayed *eventbus.Message
	err = dlq.Replay(ctx, rows[0].ID, func(_ context.Context, r ParkedRow) error {
		m, e := r.Message()
		if e != nil {
			return e
		}
		replayed = &m
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed == nil || replayed.Envelope.EventID != "evt_0" {
		t.Fatalf("replay did not reconstruct the message: %+v", replayed)
	}
	// Status now replayed; depth drops to 1.
	after, _ := dlq.Get(ctx, rows[0].ID)
	if after.Status != StatusReplayed || !after.ReplayedAt.Valid {
		t.Fatalf("row not marked replayed: %+v", after)
	}
	if d, _ := dlq.Depth(ctx, "g"); d != 1 {
		t.Fatalf("depth after replay=%d want 1", d)
	}
	// Replaying again is a no-op (idempotent operator action).
	if err := dlq.Replay(ctx, rows[0].ID, func(context.Context, ParkedRow) error {
		t.Fatal("republish must not be called for an already-replayed row")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
