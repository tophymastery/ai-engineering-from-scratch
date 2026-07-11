package index

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

func mkEnvelope(t *testing.T, eventID, eventType, merchantID string, payload any) eventbus.Envelope {
	t.Helper()
	env, err := eventbus.NewEnvelope(eventID, eventType, "trace", eventbus.Aggregate{Type: "merchant", ID: merchantID, Region: "bkk"}, 1, payload, time.Now().UTC())
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

func mkMsg(t *testing.T, topic string, env eventbus.Envelope) eventbus.Message {
	t.Helper()
	m, err := eventbus.NewMessage(topic, env)
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	return m
}

// TestConsumer_ProjectsThreeEvents verifies the search read model is built from
// menu.updated + store.status_changed + rating.updated, and that the projected
// store becomes queryable.
func TestConsumer_ProjectsThreeEvents(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	c := NewConsumer(eng, "search-projection")
	ctx := context.Background()

	menu := menuUpdatedPayload{
		MerchantID: "mer_proj", Version: 1, MerchantName: "Projected Kitchen",
		Location: &geoPoint{Lat: 13.7563, Lng: 100.5018},
		Items:    []itemField{{ItemID: "itm_1", Name: "Green Curry", Amount: 9000, Currency: "THB", Available: true}},
	}
	if err := c.Handle(ctx, mkMsg(t, TopicMenuUpdated, mkEnvelope(t, "evt_1", TopicMenuUpdated, "mer_proj", menu))); err != nil {
		t.Fatalf("menu.updated: %v", err)
	}
	if err := c.Handle(ctx, mkMsg(t, TopicStoreStatus, mkEnvelope(t, "evt_2", TopicStoreStatus, "mer_proj", storeStatusPayload{MerchantID: "mer_proj", Status: "OPEN", Version: 1}))); err != nil {
		t.Fatalf("store.status_changed: %v", err)
	}
	if err := c.Handle(ctx, mkMsg(t, TopicRatingUpdated, mkEnvelope(t, "evt_3", TopicRatingUpdated, "mer_proj", ratingUpdatedPayload{MerchantID: "mer_proj", Rating: 4.6, RatingCount: 120, Version: 1}))); err != nil {
		t.Fatalf("rating.updated: %v", err)
	}

	feed := eng.Search(Query{Lat: 13.7563, Lng: 100.5018, OpenB: true, Text: "green curry"})
	if len(feed) != 1 || feed[0].StoreID != "mer_proj" {
		t.Fatalf("projected store not queryable: %+v", feed)
	}
	if feed[0].Rating != 4.6 || feed[0].Name != "Projected Kitchen" {
		t.Fatalf("projection lost fields: %+v", feed[0])
	}
}

// TestConsumer_ExactlyOnce proves a redelivered event_id is a no-op (inbox).
func TestConsumer_ExactlyOnce(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	c := NewConsumer(eng, "search-projection")
	ctx := context.Background()

	menu := menuUpdatedPayload{MerchantID: "mer_dup", Version: 1, MerchantName: "Dup", Location: &geoPoint{Lat: 13.75, Lng: 100.50}}
	msg := mkMsg(t, TopicMenuUpdated, mkEnvelope(t, "evt_same", TopicMenuUpdated, "mer_dup", menu))
	for i := 0; i < 10; i++ {
		if err := c.Handle(ctx, msg); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}
	if c.InboxCount() != 1 {
		t.Fatalf("inbox recorded %d distinct events for 10 deliveries, want 1 (exactly-once)", c.InboxCount())
	}
}

// TestConsumer_ThroughBus wires the full Node (broker→consumer) and checks a
// published, salted menu.updated becomes queryable — the end-to-end event path.
func TestConsumer_ThroughBus(t *testing.T) {
	node := NewNode("search-projection", EngineOptions{})
	defer node.Close()
	ctx := context.Background()

	menu := menuUpdatedPayload{MerchantID: "mer_bus", Version: 1, MerchantName: "Bus Kitchen", Location: &geoPoint{Lat: 13.7563, Lng: 100.5018},
		Items: []itemField{{ItemID: "itm_1", Name: "Tom Yum", Amount: 7000, Currency: "THB", Available: true}}}
	env := mkEnvelope(t, "evt_bus_1", TopicMenuUpdated, "mer_bus", menu)
	if err := node.Publish(ctx, TopicMenuUpdated, env, SaltForDoc("itm_1")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := node.Publish(ctx, TopicStoreStatus, mkEnvelope(t, "evt_bus_2", TopicStoreStatus, "mer_bus", storeStatusPayload{MerchantID: "mer_bus", Status: "OPEN", Version: 1}), 0); err != nil {
		t.Fatalf("publish status: %v", err)
	}

	// The bus delivers asynchronously; poll briefly for the projection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		feed := node.Engine.Search(Query{Lat: 13.7563, Lng: 100.5018, OpenB: true, Text: "tom yum"})
		if len(feed) == 1 && feed[0].StoreID == "mer_bus" {
			var got menuUpdatedPayload
			_ = json.Unmarshal(env.Payload, &got)
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("published menu.updated never became queryable via the bus")
}
