package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// DLQ status values.
const (
	StatusParked   = "parked"
	StatusReplayed = "replayed"
)

// SQLDLQ is the durable, per-consumer-group dead-letter queue (D22). It
// implements eventbus.DLQSink so the bus can park a message inline after it
// exhausts retries, without blocking the partition. tools/dlqctl reads and
// replays it.
type SQLDLQ struct {
	db      *sql.DB
	dialect Dialect
	table   string
}

// NewSQLDLQ builds a DLQ store over db.
func NewSQLDLQ(db *sql.DB, d Dialect) *SQLDLQ {
	return &SQLDLQ{db: db, dialect: d, table: "dlq"}
}

// ParkedRow is one dead-lettered message.
type ParkedRow struct {
	ID         int64
	EventID    string
	Group      string
	Topic      string
	Key        string
	Payload    []byte
	Attempts   int
	Cause      string
	Status     string
	ParkedAt   time.Time
	ReplayedAt sql.NullTime
}

// Park records a parked message (eventbus.DLQSink). It is one INSERT so the
// partition worker returns immediately.
func (d *SQLDLQ) Park(ctx context.Context, msg eventbus.Message, group string, attempts int, cause error) error {
	c := ""
	if cause != nil {
		c = cause.Error()
	}
	now := time.Now().UTC()
	q := fmt.Sprintf(
		`INSERT INTO %s (event_id, consumer_group, topic, agg_key, payload, attempts, cause, status, part_day, parked_at)
		 VALUES (%s)`, d.table, phList(d.dialect, 10))
	_, err := d.db.ExecContext(ctx, q, msg.Envelope.EventID, group, msg.Topic, msg.Key, msg.Raw,
		attempts, c, StatusParked, now.Format("2006-01-02"), now)
	return err
}

// List returns parked rows, optionally filtered by group and/or status.
func (d *SQLDLQ) List(ctx context.Context, group, status string) ([]ParkedRow, error) {
	where := "1=1"
	var args []any
	if group != "" {
		args = append(args, group)
		where += fmt.Sprintf(" AND consumer_group = %s", d.dialect.Placeholder(len(args)))
	}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status = %s", d.dialect.Placeholder(len(args)))
	}
	q := fmt.Sprintf(
		`SELECT id, event_id, consumer_group, topic, agg_key, payload, attempts, cause, status, parked_at, replayed_at
		   FROM %s WHERE %s ORDER BY id ASC`, d.table, where)
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParkedRow
	for rows.Next() {
		var r ParkedRow
		if err := rows.Scan(&r.ID, &r.EventID, &r.Group, &r.Topic, &r.Key, &r.Payload,
			&r.Attempts, &r.Cause, &r.Status, &r.ParkedAt, &r.ReplayedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one parked row by id.
func (d *SQLDLQ) Get(ctx context.Context, id int64) (ParkedRow, error) {
	q := fmt.Sprintf(
		`SELECT id, event_id, consumer_group, topic, agg_key, payload, attempts, cause, status, parked_at, replayed_at
		   FROM %s WHERE id = %s`, d.table, d.dialect.Placeholder(1))
	var r ParkedRow
	err := d.db.QueryRowContext(ctx, q, id).Scan(&r.ID, &r.EventID, &r.Group, &r.Topic, &r.Key,
		&r.Payload, &r.Attempts, &r.Cause, &r.Status, &r.ParkedAt, &r.ReplayedAt)
	return r, err
}

// Depth returns the number of still-parked rows (the DLQ-depth alert metric).
func (d *SQLDLQ) Depth(ctx context.Context, group string) (int, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE status = %s`, d.table, d.dialect.Placeholder(1))
	args := []any{StatusParked}
	if group != "" {
		q += fmt.Sprintf(" AND consumer_group = %s", d.dialect.Placeholder(2))
		args = append(args, group)
	}
	var n int
	err := d.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// Republisher re-emits a parked message onto the backbone. dlqctl and the
// reference service supply one that re-inserts into the outbox, so the replayed
// event travels the normal outbox→relay→bus→inbox path and converges
// exactly-once via the consumer inbox.
type Republisher func(ctx context.Context, r ParkedRow) error

// Replay re-publishes a parked row via republish, then marks it replayed. If the
// row is already replayed it is a no-op. Exactly-once on the consumer side is
// guaranteed by the inbox even if replay runs more than once.
func (d *SQLDLQ) Replay(ctx context.Context, id int64, republish Republisher) error {
	row, err := d.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.Status == StatusReplayed {
		return nil
	}
	if err := republish(ctx, row); err != nil {
		return fmt.Errorf("dlq: replay republish id=%d: %w", id, err)
	}
	upd := fmt.Sprintf(`UPDATE %s SET status = %s, replayed_at = %s WHERE id = %s`,
		d.table, d.dialect.Placeholder(1), d.dialect.Placeholder(2), d.dialect.Placeholder(3))
	_, err = d.db.ExecContext(ctx, upd, StatusReplayed, time.Now().UTC(), id)
	return err
}

// Message reconstructs the bus message from a parked row (for republishing).
func (r ParkedRow) Message() (eventbus.Message, error) {
	env, err := eventbus.UnmarshalEnvelope(r.Payload)
	if err != nil {
		return eventbus.Message{}, err
	}
	return eventbus.Message{Topic: r.Topic, Key: r.Key, Envelope: env, Raw: r.Payload}, nil
}
