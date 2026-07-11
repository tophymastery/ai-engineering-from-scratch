package index

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
)

// consumer.go — the event projection that feeds the search read model. The
// `search` service (01 §1) "consumes menu.updated, store.status_changed,
// rating.updated" and owns a rebuildable read model (D7/D25 Tier 1). Consumption
// goes through the established inbox pattern (libs/inbox) for exactly-once
// effect. Because merchant fan-out topics are salted (`merchant_id#0..15`, D11)
// with only per-salt ordering, every projection here is LAST-WRITE-WINS by the
// event's monotonic `version` — the engine's apply paths enforce that, so events
// arriving out of order across salts converge to the same state.

// Topic names consumed (02 §4.3 / 01 §1).
const (
	TopicMenuUpdated   = "menu.updated"
	TopicStoreStatus   = "store.status_changed"
	TopicRatingUpdated = "rating.updated"
)

// menuUpdatedPayload mirrors contracts/events/menu.updated/v1.schema.json, with
// the additive-optional merchant_name + location the search index needs to place
// a store geographically (D30 additive-only; producers that omit them still
// validate).
type menuUpdatedPayload struct {
	MerchantID   string      `json:"merchant_id"`
	Version      int64       `json:"version"`
	MenuETag     string      `json:"menu_etag"`
	MerchantName string      `json:"merchant_name"`
	Location     *geoPoint   `json:"location"`
	Items        []itemField `json:"items"`
}

type geoPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type itemField struct {
	ItemID    string `json:"item_id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Available bool   `json:"available"`
}

type storeStatusPayload struct {
	MerchantID string `json:"merchant_id"`
	Status     string `json:"status"`
	Version    int64  `json:"version"`
	StatusETag string `json:"status_etag"`
}

type ratingUpdatedPayload struct {
	MerchantID  string  `json:"merchant_id"`
	Rating      float64 `json:"rating"`
	RatingCount int64   `json:"rating_count"`
	Version     int64   `json:"version"`
}

// Consumer projects the three merchant events onto an Engine with exactly-once
// effect (inbox) and LWW ordering. It is safe to run one Consumer per consumer
// group across the salted partitions.
type Consumer struct {
	eng   *Engine
	inbox *inbox.MemProcessor
}

// NewConsumer builds a projection consumer for a group. The MemProcessor is the
// in-memory exactly-once inbox (the documented high-rate stand-in for the SQL
// inbox; libs/inbox.MemProcessor gives the identical first-apply/replay-noop
// contract).
func NewConsumer(eng *Engine, group string) *Consumer {
	return &Consumer{eng: eng, inbox: inbox.NewMemProcessor(group)}
}

// Handle is the eventbus.Handler for the projection. It gives exactly-once effect
// via the inbox (a redelivered event_id is a no-op) and dispatches by event_type.
func (c *Consumer) Handle(ctx context.Context, msg eventbus.Message) error {
	_, err := c.inbox.Process(ctx, msg, func() error {
		return c.apply(msg.Envelope)
	})
	return err
}

func (c *Consumer) apply(env eventbus.Envelope) error {
	eventAt := parseTime(env.OccurredAt)
	switch env.EventType {
	case TopicMenuUpdated:
		var p menuUpdatedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return err
		}
		var lat, lng float64
		if p.Location != nil {
			lat, lng = p.Location.Lat, p.Location.Lng
		}
		items := make([]Item, 0, len(p.Items))
		for _, it := range p.Items {
			items = append(items, Item{ItemID: it.ItemID, Name: it.Name, Amount: it.Amount, Currency: it.Currency, Available: it.Available})
		}
		c.eng.IndexMerchant(MerchantDoc{
			MerchantID:  p.MerchantID,
			Name:        p.MerchantName,
			Lat:         lat,
			Lng:         lng,
			Items:       items,
			MenuVersion: p.Version,
			EventAt:     eventAt,
		})
	case TopicStoreStatus:
		var p storeStatusPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return err
		}
		c.eng.SetStoreStatus(p.MerchantID, p.Status == "OPEN", p.Version, eventAt)
	case TopicRatingUpdated:
		var p ratingUpdatedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return err
		}
		c.eng.ApplyRating(p.MerchantID, p.Rating, p.RatingCount, p.Version, eventAt)
	}
	return nil
}

// InboxCount is the number of distinct events applied (audit/dedupe proof).
func (c *Consumer) InboxCount() int { return c.inbox.Count() }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
