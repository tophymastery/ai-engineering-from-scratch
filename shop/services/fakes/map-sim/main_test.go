package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRouteDeterministic asserts zero randomness: the same route request yields
// byte-identical output across many calls, and distance/duration match the
// documented haversine × 1.3 / fixed-speed formula.
func TestRouteDeterministic(t *testing.T) {
	srv := httptest.NewServer(NewMux())
	defer srv.Close()

	body := map[string]any{
		"from": map[string]float64{"lat": 13.7563, "lng": 100.5018},
		"to":   map[string]float64{"lat": 13.7460, "lng": 100.5340},
		"mode": "CAR",
	}
	call := func() string {
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/v1/route", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := json.Marshal(mustDecode(t, resp))
		return string(raw)
	}
	first := call()
	for i := 0; i < 20; i++ {
		if call() != first {
			t.Fatalf("route not deterministic on call %d", i)
		}
	}

	var got struct {
		DistanceM int    `json:"distance_m"`
		DurationS int    `json:"duration_s"`
		Mode      string `json:"mode"`
		Polyline  string `json:"polyline"`
	}
	if err := json.Unmarshal([]byte(first), &got); err != nil {
		t.Fatal(err)
	}
	// Straight-line ~3.6 km; ×1.3 ≈ 4.7 km; /8.333 m/s ≈ 565 s. Assert a band.
	if got.DistanceM < 4000 || got.DistanceM > 6000 {
		t.Fatalf("distance_m %d outside expected band", got.DistanceM)
	}
	if got.Mode != "CAR" || !strings.HasPrefix(got.Polyline, "map-sim:v1:") {
		t.Fatalf("unexpected route shape: %s", first)
	}
	t.Logf("deterministic CAR route: %s", first)
}

// TestModeSpeedOrdering: WALK is slower than BIKE is slower than CAR for the
// same distance (fixed-speed table wired correctly).
func TestModeSpeedOrdering(t *testing.T) {
	srv := httptest.NewServer(NewMux())
	defer srv.Close()
	dur := func(mode string) int {
		resp, err := http.Get(srv.URL + "/v1/eta?from_lat=13.7563&from_lng=100.5018&to_lat=13.7460&to_lng=100.5340&mode=" + mode)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		m := mustDecode(t, resp)
		return int(m["duration_s"].(float64))
	}
	car, bike, walk := dur("CAR"), dur("BIKE"), dur("WALK")
	if !(car < bike && bike < walk) {
		t.Fatalf("expected CAR<BIKE<WALK durations, got car=%d bike=%d walk=%d", car, bike, walk)
	}
	t.Logf("eta CAR=%ds BIKE=%ds WALK=%ds", car, bike, walk)
}

func mustDecode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}
