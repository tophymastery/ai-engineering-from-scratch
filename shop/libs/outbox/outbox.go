package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// Record is one outbox row: the CDC tail id, the destination topic, the
// partition key, the raw envelope and the partition day.
type Record struct {
	ID        int64
	EventID   string
	Topic     string
	Key       string
	Raw       []byte
	CreatedAt time.Time
	Day       string // YYYY-MM-DD (UTC) — the range-partition key
}

// Message converts a Record to a bus Message (decoding the envelope once).
func (r Record) Message() (eventbus.Message, error) {
	env, err := eventbus.UnmarshalEnvelope(r.Raw)
	if err != nil {
		return eventbus.Message{}, err
	}
	return eventbus.Message{Topic: r.Topic, Key: r.Key, Envelope: env, Raw: r.Raw}, nil
}

// Source is what the Relay tails: an append-only outbox with a durable relay
// cursor. Both SQLStore and MemStore satisfy it, so the Relay is storage- and
// engine-agnostic (and a Debezium-backed source could satisfy it too).
type Source interface {
	// Tail returns records with ID > afterID ordered by ID asc, up to limit.
	// This is the CDC range scan, never a full-table poll.
	Tail(ctx context.Context, afterID int64, limit int) ([]Record, error)
	// Notify is signaled (best-effort, coalesced) when new records may exist.
	Notify() <-chan struct{}
	// LoadCursor / SaveCursor persist the per-relay published position.
	LoadCursor(ctx context.Context, relay string) (int64, error)
	SaveCursor(ctx context.Context, relay string, id int64) error
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// ---------------- SQLStore (production shape: PG; tests: SQLite) -------------

// SQLStore is the transactional outbox over database/sql.
type SQLStore struct {
	db          *sql.DB
	dialect     Dialect
	table       string
	cursorTable string
	notify      chan struct{}
}

// NewSQLStore builds a store over db. Tables default to outbox / outbox_relay_cursor.
func NewSQLStore(db *sql.DB, d Dialect) *SQLStore {
	return &SQLStore{db: db, dialect: d, table: "outbox", cursorTable: "outbox_relay_cursor", notify: make(chan struct{}, 1)}
}

// WriteInTx inserts one envelope into the outbox in the caller's transaction.
// The envelope is validated against the 02 §4.3 contract exactly once, here at
// ingress. The row commits atomically with the caller's business write.
func (s *SQLStore) WriteInTx(ctx context.Context, tx *sql.Tx, topic string, env eventbus.Envelope) error {
	raw, err := env.Marshal()
	if err != nil {
		return err
	}
	if err := eventbus.ValidateEnvelope(raw); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	now := time.Now().UTC()
	q := fmt.Sprintf(
		`INSERT INTO %s (event_id, topic, agg_key, payload, created_at, part_day) VALUES (%s)`,
		s.table, ph(s.dialect, 6))
	if _, err := tx.ExecContext(ctx, q, env.EventID, topic, env.PartitionKey(), raw, now, day(now)); err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// Signal wakes the relay. Callers invoke it after committing the outbox write so
// the relay tails promptly; it is best-effort (a coalesced non-blocking send)
// and correctness never depends on it (the relay's tick is the backstop).
func (s *SQLStore) Signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *SQLStore) Notify() <-chan struct{} { return s.notify }

func (s *SQLStore) Tail(ctx context.Context, afterID int64, limit int) ([]Record, error) {
	q := fmt.Sprintf(
		`SELECT id, event_id, topic, agg_key, payload, created_at, part_day
		   FROM %s WHERE id > %s ORDER BY id ASC LIMIT %s`,
		s.table, s.dialect.Placeholder(1), s.dialect.Placeholder(2))
	rows, err := s.db.QueryContext(ctx, q, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.EventID, &r.Topic, &r.Key, &r.Raw, &r.CreatedAt, &r.Day); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLStore) LoadCursor(ctx context.Context, relay string) (int64, error) {
	q := fmt.Sprintf(`SELECT last_id FROM %s WHERE relay_name = %s`, s.cursorTable, s.dialect.Placeholder(1))
	var id int64
	err := s.db.QueryRowContext(ctx, q, relay).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (s *SQLStore) SaveCursor(ctx context.Context, relay string, id int64) error {
	// UPSERT — portable form: try update, else insert.
	upd := fmt.Sprintf(`UPDATE %s SET last_id = %s, updated_at = %s WHERE relay_name = %s`,
		s.cursorTable, s.dialect.Placeholder(1), s.dialect.Placeholder(2), s.dialect.Placeholder(3))
	res, err := s.db.ExecContext(ctx, upd, id, time.Now().UTC(), relay)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	ins := fmt.Sprintf(`INSERT INTO %s (relay_name, last_id, updated_at) VALUES (%s)`, s.cursorTable, ph(s.dialect, 3))
	_, err = s.db.ExecContext(ctx, ins, relay, id, time.Now().UTC())
	return err
}

// DropPublishedBefore removes fully-published partition days strictly older than
// cutoff (UTC day). On PG this maps to DROP PARTITION (O(1)); the portable
// implementation here DELETEs by part_day. It refuses to drop any day that
// still holds records ahead of the relay cursor (zero event loss).
func (s *SQLStore) DropPublishedBefore(ctx context.Context, relay string, cutoff time.Time) (int, error) {
	cursor, err := s.LoadCursor(ctx, relay)
	if err != nil {
		return 0, err
	}
	cd := day(cutoff)
	// Guard: any row in an older day with id > cursor means unpublished data.
	guard := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE part_day < %s AND id > %s`,
		s.table, s.dialect.Placeholder(1), s.dialect.Placeholder(2))
	var unpub int
	if err := s.db.QueryRowContext(ctx, guard, cd, cursor).Scan(&unpub); err != nil {
		return 0, err
	}
	if unpub > 0 {
		return 0, fmt.Errorf("outbox: refusing partition drop: %d unpublished rows in days < %s", unpub, cd)
	}
	del := fmt.Sprintf(`DELETE FROM %s WHERE part_day < %s`, s.table, s.dialect.Placeholder(1))
	res, err := s.db.ExecContext(ctx, del, cd)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------- MemStore (throughput stand-in for the soak) ---------------

// MemStore is an in-memory Source with the same append-only + cursor + tail
// semantics as SQLStore, used for the high-rate soak where a single-writer
// SQLite outbox would be the bottleneck rather than the relay under test. It
// keeps the outbox transactionality contract: Append is the atomic commit.
type MemStore struct {
	mu      sync.Mutex
	log     []Record
	cursors map[string]int64
	notify  chan struct{}
	seq     int64
}

// NewMemStore builds an empty in-memory outbox.
func NewMemStore() *MemStore {
	return &MemStore{cursors: map[string]int64{}, notify: make(chan struct{}, 1)}
}

// Append commits one envelope to the outbox (the mem equivalent of WriteInTx +
// commit). It validates the envelope like the SQL path.
func (m *MemStore) Append(topic string, env eventbus.Envelope) (int64, error) {
	raw, err := env.Marshal()
	if err != nil {
		return 0, err
	}
	if err := eventbus.ValidateEnvelope(raw); err != nil {
		return 0, fmt.Errorf("outbox: %w", err)
	}
	now := time.Now().UTC()
	m.mu.Lock()
	m.seq++
	id := m.seq
	m.log = append(m.log, Record{ID: id, EventID: env.EventID, Topic: topic, Key: env.PartitionKey(), Raw: raw, CreatedAt: now, Day: day(now)})
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
	return id, nil
}

func (m *MemStore) Notify() <-chan struct{} { return m.notify }

func (m *MemStore) Tail(_ context.Context, afterID int64, limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.log { // log is id-ordered by construction
		if r.ID > afterID {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *MemStore) LoadCursor(_ context.Context, relay string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[relay], nil
}

func (m *MemStore) SaveCursor(_ context.Context, relay string, id int64) error {
	m.mu.Lock()
	m.cursors[relay] = id
	m.mu.Unlock()
	return nil
}

// DropPublishedBefore drops records with part_day < cutoff that are fully behind
// the cursor (mirrors the SQL guard: zero event loss).
func (m *MemStore) DropPublishedBefore(_ context.Context, relay string, cutoff time.Time) (int, error) {
	cd := day(cutoff)
	m.mu.Lock()
	defer m.mu.Unlock()
	cursor := m.cursors[relay]
	for _, r := range m.log {
		if r.Day < cd && r.ID > cursor {
			return 0, fmt.Errorf("outbox: refusing partition drop: unpublished row id=%d in day %s", r.ID, r.Day)
		}
	}
	kept := m.log[:0:0]
	dropped := 0
	for _, r := range m.log {
		if r.Day < cd {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	m.log = kept
	return dropped, nil
}

// Backdate rewrites a record's partition day (test helper to simulate an old
// partition without waiting a day).
func (m *MemStore) Backdate(id int64, d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.log {
		if m.log[i].ID == id {
			m.log[i].Day = d
			return
		}
	}
}

// BackdateBelow rewrites the partition day of every record with ID <= maxID,
// simulating those rows aging into an old partition. Used to exercise
// partition-drop cleanup DURING a soak (backdate the already-published tail,
// then DropPublishedBefore drops it, keeping memory flat). Returns rows touched.
func (m *MemStore) BackdateBelow(maxID int64, d string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i := range m.log {
		if m.log[i].ID <= maxID && m.log[i].Day != d {
			m.log[i].Day = d
			n++
		}
	}
	return n
}

// Len is the current number of retained records (memory-bound audit).
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.log)
}
