package main

// events.go — the two domain events this slice publishes through the
// transactional outbox (02 §4.3): `menu.updated` and `store.status_changed`,
// both keyed by `merchant_id` (aggregate id) so a merchant's catalog events stay
// ordered in one partition for the search + cart consumers. Each event carries
// the full snapshot the consumers need (no N+1 read-back, 02 §5): menu.updated
// ships every current item; store.status_changed ships the new status.
import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

const (
	menuSchemaVersion   = 1
	statusSchemaVersion = 1

	topicMenuUpdated   = "menu.updated"
	topicStoreStatus   = "store.status_changed"
	aggregateTypeMerch = "merchant"
)

// itemSnapshot is one item as carried in a menu.updated event. Prices are
// integer minor units + ISO currency (02 §1 Money); never floats.
type itemSnapshot struct {
	ItemID    string `json:"item_id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Available bool   `json:"available"`
}

// menuUpdatedPayload is the full published menu snapshot. `version` lets a
// consumer dedupe/order edits; `menu_etag` is the same validator returned to
// HTTP clients, so a consumer can correlate an event with a read.
type menuUpdatedPayload struct {
	MerchantID string         `json:"merchant_id"`
	Version    int64          `json:"version"`
	MenuETag   string         `json:"menu_etag"`
	Items      []itemSnapshot `json:"items"`
}

// storeStatusPayload is the store.status_changed snapshot.
type storeStatusPayload struct {
	MerchantID string `json:"merchant_id"`
	Status     string `json:"status"`
	Version    int64  `json:"version"`
	StatusETag string `json:"status_etag"`
}

// eventBuilder mints §4.3 envelopes for this region.
type eventBuilder struct {
	region string
}

func newEventBuilder(region string) *eventBuilder { return &eventBuilder{region: region} }

func (b *eventBuilder) menuUpdated(merchantID string, version int64, etag string, items []itemSnapshot, traceID string) eventbus.Envelope {
	if items == nil {
		items = []itemSnapshot{}
	}
	if traceID == "" {
		traceID = randTraceID()
	}
	env, _ := eventbus.NewEnvelope(
		newToken("evt"),
		topicMenuUpdated,
		traceID,
		eventbus.Aggregate{Type: aggregateTypeMerch, ID: merchantID, Region: b.region},
		menuSchemaVersion,
		menuUpdatedPayload{MerchantID: merchantID, Version: version, MenuETag: etag, Items: items},
		time.Now().UTC(),
	)
	return env
}

func (b *eventBuilder) storeStatusChanged(merchantID, status string, version int64, etag, traceID string) eventbus.Envelope {
	if traceID == "" {
		traceID = randTraceID()
	}
	env, _ := eventbus.NewEnvelope(
		newToken("evt"),
		topicStoreStatus,
		traceID,
		eventbus.Aggregate{Type: aggregateTypeMerch, ID: merchantID, Region: b.region},
		statusSchemaVersion,
		storeStatusPayload{MerchantID: merchantID, Status: status, Version: version, StatusETag: etag},
		time.Now().UTC(),
	)
	return env
}

func randTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
