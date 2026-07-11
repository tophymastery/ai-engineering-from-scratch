package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// TxHandler runs a consumer's side effects inside the SAME transaction as the
// inbox insert. Whatever it writes commits atomically with the event_id record,
// giving exactly-once effect.
type TxHandler func(ctx context.Context, tx *sql.Tx) error

// Processor is the consumer inbox over database/sql for one consumer group.
type Processor struct {
	db      *sql.DB
	dialect Dialect
	group   string
	table   string
}

// NewProcessor builds an inbox processor for a consumer group.
func NewProcessor(db *sql.DB, d Dialect, group string) *Processor {
	return &Processor{db: db, dialect: d, group: group, table: "inbox"}
}

// Process gives exactly-once effect. It opens a transaction, inserts the
// event_id (UNIQUE per group), runs the handler's side effects in the same tx,
// and commits. A duplicate delivery collides on the unique constraint and is
// skipped (applied=false, err=nil) — the handler never runs twice.
func (p *Processor) Process(ctx context.Context, msg eventbus.Message, h TxHandler) (applied bool, err error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Insert-first so the write lock is taken immediately (avoids read→write
	// upgrade deadlocks on SQLite; matches the libs/idempotency pattern).
	q := fmt.Sprintf(
		`INSERT INTO %s (event_id, consumer_group, part_day, processed_at) VALUES (%s)`,
		p.table, phList(p.dialect, 4))
	now := time.Now().UTC()
	if _, e := tx.ExecContext(ctx, q, msg.Envelope.EventID, p.group, now.Format("2006-01-02"), now); e != nil {
		if p.dialect.IsUniqueViolation(e) {
			_ = tx.Rollback()
			return false, nil // duplicate — exactly-once effect preserved
		}
		err = e
		return false, err
	}

	if h != nil {
		if e := h(ctx, tx); e != nil {
			err = e // handler failed — roll back the inbox row too, allow retry
			return false, err
		}
	}
	if e := tx.Commit(); e != nil {
		err = e
		return false, err
	}
	return true, nil
}

// NaturallyIdempotent is the documented marker (D8 skip-inbox rule) for handlers
// whose effect is inherently a no-op on re-application (UPSERT / last-write-wins
// projection). Callers embed it in the handler's doc or type to make the opt-out
// auditable, then use ProcessIdempotent instead of Process.
type NaturallyIdempotent struct{}

// ProcessIdempotent runs a naturally-idempotent handler WITHOUT an inbox row —
// the D8 opt-out. It still uses one transaction so the handler's own writes are
// atomic, but there is no dedupe record (the handler tolerates replays by
// construction). Use only where re-application is provably a no-op.
func (p *Processor) ProcessIdempotent(ctx context.Context, _ eventbus.Message, h TxHandler) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if h != nil {
		if e := h(ctx, tx); e != nil {
			_ = tx.Rollback()
			return e
		}
	}
	return tx.Commit()
}

// Seen reports whether an event_id is already recorded for this group (used by
// tests / audits).
func (p *Processor) Seen(ctx context.Context, eventID string) (bool, error) {
	q := fmt.Sprintf(`SELECT 1 FROM %s WHERE consumer_group = %s AND event_id = %s`,
		p.table, p.dialect.Placeholder(1), p.dialect.Placeholder(2))
	var one int
	err := p.db.QueryRowContext(ctx, q, p.group, eventID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// Count returns the number of inbox rows for this group (audit).
func (p *Processor) Count(ctx context.Context) (int, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE consumer_group = %s`, p.table, p.dialect.Placeholder(1))
	var n int
	err := p.db.QueryRowContext(ctx, q, p.group).Scan(&n)
	return n, err
}
