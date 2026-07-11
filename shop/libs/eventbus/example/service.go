// Command example is the S-T6 reference sandbox service: it publishes through
// the transactional outbox -> CDC relay -> eventbus and consumes through the
// exactly-once inbox, end to end, exactly as a real slice (e.g. order-service)
// would. Run it directly for a live demo of the full path; the criteria tests
// in this package (soak, dedupe, poison) drive the same wiring.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
	"github.com/shop-platform/shop/libs/outbox"
	_ "modernc.org/sqlite"
)

const topicOrderPaid = "order.paid"

// ulid is a tiny monotonic id generator standing in for the platform ULID codec
// (libs/sharding). Unique + ordered is all the backbone needs.
type ulid struct{ n atomic.Int64 }

func (u *ulid) next(prefix string) string { return fmt.Sprintf("%s_%012d", prefix, u.n.Add(1)) }

// SQLPipeline wires the full durable path over one database/sql database:
// producer (business write + outbox in one tx) -> relay -> bus -> consumer
// (inbox exactly-once + SQL DLQ). It is the reference a slice copies.
type SQLPipeline struct {
	db       *sql.DB
	store    *outbox.SQLStore
	bus      *eventbus.MemBroker
	relay    *outbox.CDCTailRelay
	proc     *inbox.Processor
	dlq      *inbox.SQLDLQ
	ids      ulid
	lag      *eventbus.LagRecorder
	consumed atomic.Int64
	group    string
}

// NewSQLPipeline builds the reference pipeline (8 partitions) on an in-memory
// SQLite database.
func NewSQLPipeline(ctx context.Context) (*SQLPipeline, error) { return newPipeline(ctx, 8) }

// newPipeline builds the reference pipeline with a chosen partition count. One
// partition is the strict head-of-line test for DLQ park-without-block.
// dbSeq gives each pipeline a distinct in-memory database so tests in one
// process don't share (and collide on) the shared-cache schema.
var dbSeq atomic.Int64

func newPipeline(ctx context.Context, partitions int) (*SQLPipeline, error) {
	dsn := fmt.Sprintf("file:example_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", dbSeq.Add(1))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := outbox.Migrate(ctx, db, outbox.SQLiteDialect{}); err != nil {
		return nil, err
	}
	if err := inbox.Migrate(ctx, db, inbox.SQLiteDialect{}); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE orders (id TEXT PRIMARY KEY, amount INTEGER);
		CREATE TABLE order_paid_view (order_id TEXT PRIMARY KEY, amount INTEGER)`); err != nil {
		return nil, err
	}
	p := &SQLPipeline{
		db:    db,
		store: outbox.NewSQLStore(db, outbox.SQLiteDialect{}),
		bus:   eventbus.NewMemBroker(eventbus.WithPartitions(partitions)),
		proc:  inbox.NewProcessor(db, inbox.SQLiteDialect{}, "projection"),
		dlq:   inbox.NewSQLDLQ(db, inbox.SQLiteDialect{}),
		lag:   eventbus.NewLagRecorder(4096),
		group: "projection",
	}
	p.relay = outbox.NewCDCTailRelay(p.store, p.bus, outbox.RelayConfig{Name: "projection", Lag: p.lag})
	return p, nil
}

// Start launches the relay and the consumer; returns a stop func.
func (p *SQLPipeline) Start(ctx context.Context, handler eventbus.Handler) (stop func(), err error) {
	rctx, rcancel := context.WithCancel(ctx)
	go p.relay.Run(rctx)
	sub, err := p.bus.Subscribe(eventbus.SubscribeConfig{
		Topic: topicOrderPaid, Group: p.group, MaxAttempts: 3, DLQ: p.dlq,
	}, handler)
	if err != nil {
		rcancel()
		return nil, err
	}
	return func() { sub.Close(); rcancel() }, nil
}

// DefaultConsumer builds the projection consumer: exactly-once insert into
// order_paid_view via the inbox.
func (p *SQLPipeline) DefaultConsumer() eventbus.Handler {
	return func(ctx context.Context, m eventbus.Message) error {
		applied, err := p.proc.Process(ctx, m, func(ctx context.Context, tx *sql.Tx) error {
			var amt struct {
				Amount int `json:"amount"`
			}
			_ = jsonUnmarshal(m.Envelope.Payload, &amt)
			_, e := tx.ExecContext(ctx, `INSERT INTO order_paid_view (order_id, amount) VALUES (?, ?)`,
				m.Envelope.Aggregate.ID, amt.Amount)
			return e
		})
		if err != nil {
			return err
		}
		if applied {
			p.consumed.Add(1)
		}
		return nil
	}
}

// PublishOrder writes an order + its order.paid outbox row in one transaction,
// then wakes the relay. Returns the event id.
func (p *SQLPipeline) PublishOrder(ctx context.Context, amount int) (string, error) {
	orderID := p.ids.next("ord")
	eventID := p.ids.next("evt")
	env, err := eventbus.NewEnvelope(eventID, topicOrderPaid, "trace_"+eventID,
		eventbus.Aggregate{Type: "order", ID: orderID, Region: "bkk"}, 3,
		map[string]any{"order_id": orderID, "amount": amount}, time.Time{})
	if err != nil {
		return "", err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO orders (id, amount) VALUES (?, ?)`, orderID, amount); err != nil {
		_ = tx.Rollback()
		return "", err
	}
	if err := p.store.WriteInTx(ctx, tx, topicOrderPaid, env); err != nil {
		_ = tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	p.store.Signal()
	return eventID, nil
}

// Audit returns (orders written, projection rows, relay published, consumed).
func (p *SQLPipeline) Audit(ctx context.Context) (orders, view int, published, consumed int64) {
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders`).Scan(&orders)
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_paid_view`).Scan(&view)
	return orders, view, p.relay.Published(), p.consumed.Load()
}

// RepublishViaOutbox re-inserts a parked event's envelope into the outbox in a
// fresh transaction, so a DLQ replay travels the normal outbox->relay->bus->inbox
// path and converges exactly-once through the inbox. tools/dlqctl uses the same
// logic; this method is the in-process equivalent for the poison test.
func (p *SQLPipeline) RepublishViaOutbox(ctx context.Context, r inbox.ParkedRow) error {
	env, err := eventbus.UnmarshalEnvelope(r.Payload)
	if err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := p.store.WriteInTx(ctx, tx, r.Topic, env); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.store.Signal()
	return nil
}

func (p *SQLPipeline) LagP99() time.Duration   { return p.lag.Quantile(0.99) }
func (p *SQLPipeline) DB() *sql.DB             { return p.db }
func (p *SQLPipeline) DLQ() *inbox.SQLDLQ      { return p.dlq }
func (p *SQLPipeline) Store() *outbox.SQLStore { return p.store }

func main() {
	ctx := context.Background()
	p, err := NewSQLPipeline(ctx)
	if err != nil {
		panic(err)
	}
	stop, err := p.Start(ctx, p.DefaultConsumer())
	if err != nil {
		panic(err)
	}
	defer stop()

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := p.PublishOrder(ctx, 100+i); err != nil {
				panic(err)
			}
		}(i)
	}
	wg.Wait()

	deadline := time.After(10 * time.Second)
	for {
		if _, _, _, consumed := p.Audit(ctx); consumed >= n {
			break
		}
		select {
		case <-deadline:
			fmt.Println("demo: timed out waiting for consumption")
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
	orders, view, published, consumed := p.Audit(ctx)
	fmt.Printf("reference svc: orders=%d published=%d consumed=%d projection=%d lag_p99=%s\n",
		orders, published, consumed, view, p.LagP99())
	fmt.Println("outbox -> relay(CDC tail) -> eventbus -> inbox(exactly-once) OK")
}
