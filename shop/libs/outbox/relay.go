package outbox

import (
	"context"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

// Relay is the CDC abstraction: it tails a Source and publishes to the bus,
// advancing a durable cursor. Production swaps CDCTailRelay for Debezium
// (deploy/cdc/debezium-connector.json) behind this same behavior.
type Relay interface {
	// Run publishes until ctx is cancelled. It returns ctx.Err() on shutdown.
	Run(ctx context.Context) error
}

// CDCTailRelay tails a Source by monotonic id and publishes to a Publisher.
type CDCTailRelay struct {
	name  string
	src   Source
	pub   eventbus.Publisher
	batch int
	tick  time.Duration
	lag   *eventbus.LagRecorder
	// published counts messages published (audit for the offset/count audit).
	published *int64Counter
}

// RelayConfig configures a CDCTailRelay.
type RelayConfig struct {
	Name  string        // durable cursor name (per relay/consumer path)
	Batch int           // max records per tail read (default 500)
	Tick  time.Duration // backstop wake interval (default 20ms) — a bounded
	// tail read, NOT a full-table poll (see package doc).
	Lag *eventbus.LagRecorder // optional: records write→publish lag per event
}

// NewCDCTailRelay builds a relay over src publishing to pub.
func NewCDCTailRelay(src Source, pub eventbus.Publisher, cfg RelayConfig) *CDCTailRelay {
	if cfg.Batch <= 0 {
		cfg.Batch = 500
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 20 * time.Millisecond
	}
	if cfg.Name == "" {
		cfg.Name = "relay"
	}
	return &CDCTailRelay{name: cfg.Name, src: src, pub: pub, batch: cfg.Batch, tick: cfg.Tick, lag: cfg.Lag, published: &int64Counter{}}
}

// Published is the number of messages this relay has published (audit).
func (r *CDCTailRelay) Published() int64 { return r.published.get() }

// Run tails and publishes until ctx is cancelled.
func (r *CDCTailRelay) Run(ctx context.Context) error {
	cursor, err := r.src.LoadCursor(ctx, r.name)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	for {
		// Drain everything currently available (repeated bounded tail reads).
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			recs, err := r.src.Tail(ctx, cursor, r.batch)
			if err != nil {
				return err
			}
			if len(recs) == 0 {
				break
			}
			msgs := make([]eventbus.Message, 0, len(recs))
			for _, rec := range recs {
				m, err := rec.Message()
				if err != nil {
					// A malformed row can't be published; skip it past the
					// cursor so it never wedges the relay (it stays in the
					// outbox for inspection until partition drop).
					cursor = rec.ID
					continue
				}
				msgs = append(msgs, m)
			}
			if len(msgs) > 0 {
				// Publish first, THEN advance the cursor: a crash in between
				// republishes (at-least-once) and the inbox dedupes.
				if err := r.pub.Publish(ctx, msgs...); err != nil {
					return err
				}
				if r.lag != nil {
					now := time.Now()
					for _, rec := range recs {
						r.lag.Record(now.Sub(rec.CreatedAt))
					}
				}
				r.published.add(int64(len(msgs)))
			}
			cursor = recs[len(recs)-1].ID
			if err := r.src.SaveCursor(ctx, r.name, cursor); err != nil {
				return err
			}
		}
		// Nothing left; wait for a signal or the backstop tick.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.src.Notify():
		case <-ticker.C:
		}
	}
}
