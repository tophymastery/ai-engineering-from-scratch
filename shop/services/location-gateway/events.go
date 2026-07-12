package main

import (
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
	plane "github.com/shop-platform/shop/services/location-gateway/plane"
)

// events.go — the topic vocabulary + envelope helper for the telemetry plane.
//
// PRODUCED (02 §4.3, key region:driver_id, on the telemetry cluster — D5/D14):
//   - driver.location_updated → a batched, sampled driver position. tracking →
//     dispatch (its kNN read) + customer live-tracking. D30 additive-only.
//
// The gateway batches 100 ms windows of frames (D14); on flush the telemetry sink
// updates the H3 geo index (D15) and emits ONE driver.location_updated per
// position onto the telemetry topic (the sampled event other slices consume).
const (
	TopicDriverLocation = "driver.location_updated"
)

// makeLocationEnvelope builds a driver.location_updated envelope about a driver
// (aggregate type "driver", key region:driver_id — 02 §4.3). Payload mirrors
// contracts/events/driver.location_updated/v1.schema.json (driver_id, h3_cell,
// lat, lng, recorded_at).
func makeLocationEnvelope(driverID, region string, cell plane.Cell, lat, lng float64, recordedAt time.Time) (eventbus.Envelope, error) {
	return eventbus.NewEnvelope(
		newToken("evt"), TopicDriverLocation, "trace_"+driverID,
		eventbus.Aggregate{Type: "driver", ID: driverID, Region: region},
		1,
		map[string]any{
			"driver_id":   driverID,
			"h3_cell":     cell.Key(),
			"lat":         lat,
			"lng":         lng,
			"recorded_at": recordedAt.UTC().Format(time.RFC3339),
		},
		recordedAt,
	)
}
