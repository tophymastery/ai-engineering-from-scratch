package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/shop-platform/shop/libs/idempotency"
)

// timers.go is the DURABLE TIMER TABLE + LEASED SWEEPER — the mechanism behind
// the V-T9 crash test: "kill all order pods with 1k pending timers ⇒ 100% fire
// within 60s of due." Timers live in the `timers` table (store.go), NOT in
// process memory, so a crash loses only the in-memory sweeper goroutine; on
// restart a fresh sweeper reclaims every due row from the table and fires it.
//
// The LEASE (leased_by / leased_until + the `status='PENDING'` guard on the
// claim UPDATE) makes firing EXACTLY-ONCE even with N sweepers racing: each due
// row is flipped PENDING→FIRING by exactly one sweeper's guarded UPDATE; the
// losers' UPDATE affects 0 rows. This is the same optimistic-claim pattern a
// production PG sweeper uses (SELECT … FOR UPDATE SKIP LOCKED); on the in-memory
// SQLite single-writer it is enforced by the guarded UPDATE + serialised writes.

// Timer kinds (01 §4 "Timeouts drive the saga forward too").
const (
	KindRemediation = "payment_remediation" // PAYMENT_PENDING > 15 min ⇒ void + cancel
	KindAccept      = "t_accept"            // PAID, merchant hasn't accepted ⇒ refund + cancel
	KindDispatch    = "t_dispatch"          // ACCEPTED, no driver ⇒ refund + cancel
	KindCaptureBy   = "capture_by"          // DELIVERED ⇒ capture + settlement accrual
)

// Default timer windows (01 §4). PAYMENT_PENDING remediation is fixed at 15 min
// (the test criterion: auto-void in < 16 min).
const (
	DefaultRemediationWindow = 15 * time.Minute
	DefaultAcceptWindow      = 10 * time.Minute
	DefaultDispatchWindow    = 8 * time.Minute
	DefaultCaptureByWindow   = 30 * time.Minute
)

// TimerRow is one durable timer.
type TimerRow struct {
	TimerID string
	OrderID string
	Kind    string
	Trigger Trigger
	DueAt   time.Time
	Status  string
}

// --- scheduling (inside a tx, atomic with the state change that arms it) -----

// scheduleTimerTx arms a timer inside the caller's idempotent tx (checkout path).
func (s *store) scheduleTimerTx(ctx context.Context, tx idempotency.Execer, orderID, kind string, trig Trigger, dueAt time.Time) (string, error) {
	id := newToken("tmr")
	_, err := tx.Exec(ctx,
		`INSERT INTO timers (timer_id, order_id, kind, trigger, due_at, status, created_at)
		 VALUES (?, ?, ?, ?, ?, 'PENDING', ?)`,
		id, orderID, kind, string(trig), dueAt, s.clock.Now())
	return id, err
}

// scheduleTimerSQLTx arms a timer inside a raw *sql.Tx (saga transition path).
func (s *store) scheduleTimerSQLTx(ctx context.Context, tx *sql.Tx, orderID, kind string, trig Trigger, dueAt time.Time) (string, error) {
	id := newToken("tmr")
	_, err := tx.ExecContext(ctx,
		`INSERT INTO timers (timer_id, order_id, kind, trigger, due_at, status, created_at)
		 VALUES (?, ?, ?, ?, ?, 'PENDING', ?)`,
		id, orderID, kind, string(trig), dueAt, s.clock.Now())
	return id, err
}

// cancelTimersSQLTx cancels the PENDING timers of a kind for an order (called
// when the order leaves the state that armed them — e.g. PAID cancels the
// remediation timer; ACCEPTED cancels T_accept). Idempotent.
func (s *store) cancelTimersSQLTx(ctx context.Context, tx *sql.Tx, orderID, kind string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE timers SET status='CANCELLED' WHERE order_id=? AND kind=? AND status='PENDING'`,
		orderID, kind)
	return err
}

// --- claiming + firing (the leased sweeper) ----------------------------------

// ClaimDueTimers atomically leases up to `limit` PENDING timers whose due_at has
// passed and whose lease is free/expired, flipping them PENDING→FIRING under
// this sweeper's id. Returns the rows THIS sweeper won. The `status='PENDING'`
// guard on the UPDATE is what makes each row claimable by exactly one sweeper.
func (s *store) ClaimDueTimers(ctx context.Context, sweeperID string, now time.Time, lease time.Duration, limit int) ([]TimerRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT timer_id, order_id, kind, trigger, due_at FROM timers
		  WHERE status='PENDING' AND due_at <= ? AND (leased_until IS NULL OR leased_until < ?)
		  ORDER BY due_at ASC LIMIT ?`,
		now, now, limit)
	if err != nil {
		return nil, err
	}
	var cands []TimerRow
	for rows.Next() {
		var t TimerRow
		var trig string
		if err := rows.Scan(&t.TimerID, &t.OrderID, &t.Kind, &trig, &t.DueAt); err != nil {
			rows.Close()
			return nil, err
		}
		t.Trigger = Trigger(trig)
		cands = append(cands, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	leaseUntil := now.Add(lease)
	claimed := make([]TimerRow, 0, len(cands))
	for _, t := range cands {
		res, err := tx.ExecContext(ctx,
			`UPDATE timers SET status='FIRING', leased_by=?, leased_until=?
			  WHERE timer_id=? AND status='PENDING'`,
			sweeperID, leaseUntil, t.TimerID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			t.Status = "FIRING"
			claimed = append(claimed, t)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

// markTimerFired records a successful fire (terminal).
func (s *store) markTimerFired(ctx context.Context, timerID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE timers SET status='FIRED', fired_at=? WHERE timer_id=?`, at, timerID)
	return err
}

// releaseTimer returns a claimed-but-failed timer to PENDING so a later sweep
// retries it (the fire effect errored — e.g. a transient store error).
func (s *store) releaseTimer(ctx context.Context, timerID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE timers SET status='PENDING', leased_by='', leased_until=NULL WHERE timer_id=? AND status='FIRING'`,
		timerID)
	return err
}

// --- audit counters (crash-test assertions) ---------------------------------

func (s *store) timerCountByStatus(ctx context.Context, status string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM timers WHERE status=?`, status).Scan(&n)
	return n, err
}

func (s *store) pendingDueCount(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM timers WHERE status='PENDING' AND due_at <= ?`, now).Scan(&n)
	return n, err
}

// --- Sweeper -----------------------------------------------------------------

// FireFunc applies a fired timer's effect (in the real service, the saga trigger
// + its compensation; in the pure-timer crash test, a counter). Returning an
// error releases the lease so the timer is retried on the next sweep.
type FireFunc func(ctx context.Context, t TimerRow) error

// Sweeper drains due timers from the durable table and fires them under a lease.
// It holds NO durable state itself — a crash drops the Sweeper; a new one over
// the same store reclaims every due row. That is the crash-survival property.
type Sweeper struct {
	st    *store
	id    string
	lease time.Duration
	batch int
	clock Clock
	fire  FireFunc
}

// NewSweeper builds a sweeper. lease bounds how long a claimed-but-not-yet-fired
// timer is invisible to other sweepers; batch bounds rows per claim round.
func NewSweeper(st *store, id string, clock Clock, fire FireFunc) *Sweeper {
	return &Sweeper{st: st, id: id, lease: 30 * time.Second, batch: 500, clock: clock, fire: fire}
}

// SweepOnce drains ALL currently-due timers (looping claim rounds until none
// remain) and fires each. Returns the number fired this sweep. Deterministic
// under a frozen clock: it fires exactly the timers due at clock.Now().
func (sw *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	total := 0
	for {
		now := sw.clock.Now()
		claimed, err := sw.st.ClaimDueTimers(ctx, sw.id, now, sw.lease, sw.batch)
		if err != nil {
			return total, err
		}
		if len(claimed) == 0 {
			return total, nil
		}
		for _, t := range claimed {
			if err := sw.fire(ctx, t); err != nil {
				_ = sw.st.releaseTimer(ctx, t.TimerID)
				continue
			}
			if err := sw.st.markTimerFired(ctx, t.TimerID, now); err != nil {
				return total, err
			}
			total++
		}
	}
}

// Run sweeps every tick until ctx is cancelled. tick MUST be < 60s so a due
// timer fires within 60s of due (the crash-test SLO). Production default 5s.
func (sw *Sweeper) Run(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = sw.SweepOnce(ctx)
		}
	}
}
