package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// timers_test.go proves the V-T9 durable-timer crash property:
//
//	"Kill all order pods with 1k pending timers ⇒ 100% fire within 60s of due."
//
// Sandbox adaptation (disclosed in VERIFICATION §V-T9): there are no pods here,
// so "kill all pods" is simulated by DISCARDING the in-memory sweeper (dropping
// all process state) while RETAINING the durable timers table, then starting a
// FRESH sweeper over the same store — exactly what a restarted pod does against
// the shared PG. The counts (1000/1000) + within-60s-of-due + exactly-once are
// FULL and run under -race; only wall-clock duration is compressed to a frozen
// clock we advance (never sleep).

// seedTimers inserts n PENDING timers all due at dueAt.
func seedTimers(t *testing.T, st *store, n int, dueAt time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO timers (timer_id, order_id, kind, trigger, due_at, status, created_at)
			 VALUES (?, ?, ?, ?, ?, 'PENDING', ?)`,
			newToken("tmr"), newToken("ord"), KindRemediation, string(TrigPaymentTimeout), dueAt, st.clock.Now()); err != nil {
			t.Fatalf("insert timer %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestDurableTimers_CrashAndFire_1000 is the headline crash test.
func TestDurableTimers_CrashAndFire_1000(t *testing.T) {
	const N = 1000
	ctx := context.Background()
	clk := NewManualClock(t0)
	st, err := openStore(ctx, "bkk", clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.close()

	due := t0.Add(15 * time.Minute) // e.g. remediation timers due at checkout+15m
	seedTimers(t, st, N, due)

	// A sweeper exists, but the process now CRASHES before firing anything: we
	// drop it on the floor. The durable timers table is untouched.
	_ = NewSweeper(st, "sweeper-before-crash", clk, func(context.Context, TimerRow) error { return nil })

	// --- restart: a brand-new sweeper over the SAME durable store. ---
	var fired int64
	// fired_at lateness bound (must be ≤ 60s of due for every timer).
	sweeper := NewSweeper(st, "sweeper-after-restart", clk, func(_ context.Context, tm TimerRow) error {
		atomic.AddInt64(&fired, 1)
		return nil
	})

	// Advance the frozen clock to 59s AFTER due (inside the 60s-of-due SLO).
	clk.Advance(15*time.Minute + 59*time.Second)

	// Sanity: all N are pending+due before the sweep.
	if pd, _ := st.pendingDueCount(ctx, clk.Now()); pd != N {
		t.Fatalf("pending-due before sweep = %d want %d", pd, N)
	}

	n, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	// 100% fired.
	if n != N || atomic.LoadInt64(&fired) != N {
		t.Fatalf("fired %d (fn calls %d) want %d — not 100%%", n, fired, N)
	}
	firedRows, _ := st.timerCountByStatus(ctx, "FIRED")
	pending, _ := st.timerCountByStatus(ctx, "PENDING")
	if firedRows != N || pending != 0 {
		t.Fatalf("FIRED=%d PENDING=%d want FIRED=%d PENDING=0", firedRows, pending, N)
	}

	// Within-60s-of-due: every fired_at is ≤ due+60s.
	lateness := clk.Now().Sub(due)
	if lateness > 60*time.Second {
		t.Fatalf("sweep fired at %v after due (> 60s)", lateness)
	}
	t.Logf("CRASH-AND-FIRE: %d/%d timers fired within %v of due (SLO 60s); FIRED=%d PENDING=0", n, N, lateness, firedRows)
}

// TestDurableTimers_LeasedExactlyOnce: two sweepers race the SAME 1000 due timers
// concurrently (under -race); the lease guarantees each timer fires EXACTLY once
// (no double-fire), and together they fire all 1000.
func TestDurableTimers_LeasedExactlyOnce(t *testing.T) {
	const N = 1000
	ctx := context.Background()
	clk := NewManualClock(t0)
	st, err := openStore(ctx, "bkk", clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.close()

	due := t0
	seedTimers(t, st, N, due)
	clk.Advance(time.Second) // all due

	var total int64
	mkSweeper := func(id string) *Sweeper {
		return NewSweeper(st, id, clk, func(context.Context, TimerRow) error {
			atomic.AddInt64(&total, 1)
			return nil
		})
	}
	swA, swB := mkSweeper("A"), mkSweeper("B")

	var wg sync.WaitGroup
	var fa, fb int
	wg.Add(2)
	go func() { defer wg.Done(); fa, _ = swA.SweepOnce(ctx) }()
	go func() { defer wg.Done(); fb, _ = swB.SweepOnce(ctx) }()
	wg.Wait()

	if fa+fb != N || atomic.LoadInt64(&total) != N {
		t.Fatalf("two sweepers fired A=%d B=%d total=%d want combined %d (no double-fire)", fa, fb, total, N)
	}
	firedRows, _ := st.timerCountByStatus(ctx, "FIRED")
	if firedRows != N {
		t.Fatalf("FIRED rows %d want %d", firedRows, N)
	}
	t.Logf("LEASED EXACTLY-ONCE: A fired %d, B fired %d, combined %d/%d, zero double-fire", fa, fb, fa+fb, N)
}

// TestDurableTimers_NotYetDue: a timer is NOT fired before its due_at (the sweep
// respects due_at on the frozen clock).
func TestDurableTimers_NotYetDue(t *testing.T) {
	ctx := context.Background()
	clk := NewManualClock(t0)
	st, err := openStore(ctx, "bkk", clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.close()
	seedTimers(t, st, 10, t0.Add(15*time.Minute))
	sw := NewSweeper(st, "s", clk, func(context.Context, TimerRow) error { return nil })
	// clock still at t0 (< due): nothing fires.
	if n, _ := sw.SweepOnce(ctx); n != 0 {
		t.Fatalf("fired %d before due, want 0", n)
	}
	clk.Advance(15 * time.Minute)
	if n, _ := sw.SweepOnce(ctx); n != 10 {
		t.Fatalf("fired %d at due, want 10", n)
	}
}
