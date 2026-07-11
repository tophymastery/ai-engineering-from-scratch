package main

// events.go — the ONE event this slice CONSUMES (02 §4.3): `menu.updated`, keyed
// by `merchant_id`, produced by merchant-catalog (V-T3). Cart is a consumer, not
// a producer. Consumption goes through the established inbox pattern
// (libs/inbox.MemProcessor — the in-memory exactly-once inbox V-T4/V-T5 use as
// the high-rate stand-in for the SQL inbox; identical first-apply/replay-noop
// contract) so a redelivered event_id is a no-op. Applying a menu.updated:
//   1. installs the new menu snapshot into catalogView (LWW by version), then
//   2. revalidates every cart line referencing that merchant — reprice / flag
//      unavailable — so a merchant's price or availability change is reflected in
//      affected carts within the freshness window (< 5 s). Real event→reflected.
import (
	"context"
	"encoding/json"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
)

// TopicMenuUpdated is the topic cart subscribes to (02 §4.3 / 01 §1).
const TopicMenuUpdated = "menu.updated"

// menuUpdatedPayload mirrors contracts/events/menu.updated/v1.schema.json. The
// item price is FLATTENED in the event payload (amount/currency) — the same
// numbers the HTTP menu read nests under `price` (catalog.go). The additive
// merchant_name/location fields (D30, V-T4) are ignored by cart.
type menuUpdatedPayload struct {
	MerchantID string           `json:"merchant_id"`
	Version    int64            `json:"version"`
	MenuETag   string           `json:"menu_etag"`
	Items      []menuItemField  `json:"items"`
}

type menuItemField struct {
	ItemID    string `json:"item_id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Available bool   `json:"available"`
}

// menuConsumer projects menu.updated onto catalogView with exactly-once effect
// and triggers cart revalidation. onApplied is called after the view is updated,
// with the merchant whose menu changed, so the store can reprice affected carts.
type menuConsumer struct {
	view      *catalogView
	inbox     *inbox.MemProcessor
	onApplied func(ctx context.Context, merchantID string, version int64) error
}

// newMenuConsumer builds the projection consumer for a group.
func newMenuConsumer(view *catalogView, group string, onApplied func(ctx context.Context, merchantID string, version int64) error) *menuConsumer {
	return &menuConsumer{view: view, inbox: inbox.NewMemProcessor(group), onApplied: onApplied}
}

// Handle is the eventbus.Handler. It gives exactly-once effect via the inbox (a
// redelivered event_id is a no-op) and applies the menu snapshot + revalidation.
func (c *menuConsumer) Handle(ctx context.Context, msg eventbus.Message) error {
	_, err := c.inbox.Process(ctx, msg, func() error {
		return c.apply(ctx, msg.Envelope)
	})
	return err
}

func (c *menuConsumer) apply(ctx context.Context, env eventbus.Envelope) error {
	if env.EventType != TopicMenuUpdated {
		return nil // not ours; ignore (forward compat)
	}
	var p menuUpdatedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return err
	}
	items := make(map[string]itemInfo, len(p.Items))
	for _, it := range p.Items {
		items[it.ItemID] = itemInfo{
			Name: it.Name, Amount: it.Amount, Currency: it.Currency, Available: it.Available,
		}
	}
	if !c.view.applyMenu(p.MerchantID, p.Version, items) {
		return nil // stale/duplicate snapshot (LWW) — nothing to revalidate
	}
	if c.onApplied != nil {
		return c.onApplied(ctx, p.MerchantID, p.Version)
	}
	return nil
}

// InboxCount is the number of distinct events applied (audit/dedupe proof).
func (c *menuConsumer) InboxCount() int { return c.inbox.Count() }
