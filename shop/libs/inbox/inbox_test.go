package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := Migrate(context.Background(), db, SQLiteDialect{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func msg(t *testing.T, id string) eventbus.Message {
	t.Helper()
	env, err := eventbus.NewEnvelope(id, "order.paid", "tr", eventbus.Aggregate{Type: "order", ID: "ord_1"}, 1, map[string]any{"n": 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := eventbus.NewMessage("order.paid", env)
	return m
}

// TestExactlyOnceEffect: redelivering the same event 10x yields one side effect.
func TestExactlyOnceEffect(t *testing.T) {
	db := openDB(t)
	p := NewProcessor(db, SQLiteDialect{}, "g")
	ctx := context.Background()
	var effects atomic.Int64

	m := msg(t, "evt_dup")
	for i := 0; i < 10; i++ {
		applied, err := p.Process(ctx, m, func(_ context.Context, tx *sql.Tx) error {
			effects.Add(1)
			_, e := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sink (k TEXT)`)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO sink (k) VALUES (?)`, m.Envelope.EventID)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && !applied {
			t.Fatal("first delivery should apply")
		}
		if i > 0 && applied {
			t.Fatalf("delivery %d applied a duplicate", i)
		}
	}
	if effects.Load() != 1 {
		t.Fatalf("effects=%d want 1", effects.Load())
	}
	var rows int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sink`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("sink rows=%d want 1", rows)
	}
}

// TestConcurrentDuplicateBurst: 10 goroutines process the same event
// concurrently => exactly one effect (unique constraint arbitrates).
func TestConcurrentDuplicateBurst(t *testing.T) {
	db := openDB(t)
	p := NewProcessor(db, SQLiteDialect{}, "g")
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE sink (k TEXT)`); err != nil {
		t.Fatal(err)
	}
	m := msg(t, "evt_race")
	var wg sync.WaitGroup
	var applied atomic.Int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := p.Process(ctx, m, func(_ context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, `INSERT INTO sink (k) VALUES (?)`, m.Envelope.EventID)
				return e
			})
			if err != nil {
				t.Errorf("process: %v", err)
			}
			if ok {
				applied.Add(1)
			}
		}()
	}
	wg.Wait()
	if applied.Load() != 1 {
		t.Fatalf("applied=%d want exactly 1", applied.Load())
	}
	var rows int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sink`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("sink rows=%d want 1", rows)
	}
}

// TestHandlerFailureRollsBack: a failing handler leaves no inbox row, so a later
// retry can still apply (at-least-once + exactly-once effect).
func TestHandlerFailureRollsBack(t *testing.T) {
	db := openDB(t)
	p := NewProcessor(db, SQLiteDialect{}, "g")
	ctx := context.Background()
	m := msg(t, "evt_fail")

	_, err := p.Process(ctx, m, func(_ context.Context, _ *sql.Tx) error { return fmt.Errorf("boom") })
	if err == nil {
		t.Fatal("expected handler error to propagate")
	}
	seen, _ := p.Seen(ctx, "evt_fail")
	if seen {
		t.Fatal("failed handler must not leave an inbox row")
	}
	// Retry succeeds and applies.
	applied, err := p.Process(ctx, m, func(_ context.Context, _ *sql.Tx) error { return nil })
	if err != nil || !applied {
		t.Fatalf("retry applied=%v err=%v", applied, err)
	}
}

// TestSkipInboxRule: ProcessIdempotent runs the handler without recording an
// inbox row (the D8 opt-out for naturally-idempotent handlers).
func TestSkipInboxRule(t *testing.T) {
	db := openDB(t)
	p := NewProcessor(db, SQLiteDialect{}, "g")
	ctx := context.Background()
	m := msg(t, "evt_idem")
	var calls atomic.Int64
	upsert := func(_ context.Context, _ *sql.Tx) error { calls.Add(1); return nil } // NaturallyIdempotent
	for i := 0; i < 3; i++ {
		if err := p.ProcessIdempotent(ctx, m, upsert); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("idempotent handler calls=%d want 3 (no dedupe)", calls.Load())
	}
	if n, _ := p.Count(ctx); n != 0 {
		t.Fatalf("skip-inbox left %d rows, want 0", n)
	}
}

func TestInboxRetentionDrop(t *testing.T) {
	db := openDB(t)
	p := NewProcessor(db, SQLiteDialect{}, "g")
	ctx := context.Background()
	// One current, one 10-day-old row.
	if _, err := p.Process(ctx, msg(t, "fresh"), func(context.Context, *sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	if _, err := db.Exec(`INSERT INTO inbox (event_id, consumer_group, part_day, processed_at) VALUES ('old','g',?,?)`, old, time.Now()); err != nil {
		t.Fatal(err)
	}
	n, err := DropInboxOlderThanRetention(ctx, db, SQLiteDialect{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("dropped %d, want 1 (the 10-day-old row)", n)
	}
	if cnt, _ := p.Count(ctx); cnt != 1 {
		t.Fatalf("remaining=%d want 1 (fresh)", cnt)
	}
}
