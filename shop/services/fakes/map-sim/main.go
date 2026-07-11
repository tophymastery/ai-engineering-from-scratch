// Command map-sim is the S-T7 deterministic routing/ETA fake (03 §5). It returns
// distance = haversine(from,to) × 1.3 road factor and duration = distance ÷ a
// fixed per-mode speed, plus a polyline stub — with ZERO randomness, so dispatch
// and tracking tests get byte-identical routes every run and never call a paid
// maps API. Std-lib only.
//
// Endpoints (02 §1 canonical /v1 + bare task aliases):
//
//	POST /v1/route  (alias /route)   {from:{lat,lng}, to:{lat,lng}, mode}
//	GET  /v1/eta    (alias /eta)     ?from_lat&from_lng&to_lat&to_lng&mode
//	GET  /healthz
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
)

// roadFactor inflates straight-line distance to approximate a road path (03 §5).
const roadFactor = 1.3

// speedMPS is the fixed ground speed per mode (metres/second). Deterministic.
var speedMPS = map[string]float64{
	"CAR":  8.333, // ~30 km/h city
	"BIKE": 5.556, // ~20 km/h
	"WALK": 1.389, // ~5 km/h
}

func main() {
	port := envOr("PORT", "8092")
	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1 (container healthcheck)")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}
	addr := ":" + port
	log.Printf("map-sim on %s (roadFactor=%.1f)", addr, roadFactor)
	if err := http.ListenAndServe(addr, NewMux()); err != nil {
		log.Fatalf("map-sim exited: %v", err)
	}
}

type point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// NewMux wires the deterministic routing surface.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "map-sim"})
	})

	route := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed", "", "")
			return
		}
		var in struct {
			From point  `json:"from"`
			To   point  `json:"to"`
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, r, http.StatusBadRequest, "body must be valid JSON", "body", "invalid_json")
			return
		}
		mode, ok := normMode(in.Mode)
		if !ok {
			writeErr(w, r, http.StatusBadRequest, "mode must be CAR, BIKE or WALK", "mode", "invalid_enum")
			return
		}
		if e := validate(in.From, in.To); e != "" {
			writeErr(w, r, http.StatusBadRequest, e, "from", "out_of_range")
			return
		}
		dist, dur := compute(in.From, in.To, mode)
		writeJSON(w, http.StatusOK, map[string]any{
			"distance_m": dist,
			"duration_s": dur,
			"mode":       mode,
			"polyline":   polyline(in.From, in.To),
		})
	}

	eta := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed", "", "")
			return
		}
		q := r.URL.Query()
		from := point{Lat: qFloat(q, "from_lat"), Lng: qFloat(q, "from_lng")}
		to := point{Lat: qFloat(q, "to_lat"), Lng: qFloat(q, "to_lng")}
		mode, ok := normMode(q.Get("mode"))
		if !ok {
			writeErr(w, r, http.StatusBadRequest, "mode must be CAR, BIKE or WALK", "mode", "invalid_enum")
			return
		}
		if e := validate(from, to); e != "" {
			writeErr(w, r, http.StatusBadRequest, e, "from_lat", "out_of_range")
			return
		}
		dist, dur := compute(from, to, mode)
		writeJSON(w, http.StatusOK, map[string]any{"distance_m": dist, "duration_s": dur, "mode": mode})
	}

	for _, base := range []string{"/v1", ""} {
		mux.HandleFunc(base+"/route", route)
		mux.HandleFunc(base+"/eta", eta)
	}
	return mux
}

// compute is the deterministic core: haversine × road factor, ÷ fixed speed.
func compute(from, to point, mode string) (distanceM, durationS int) {
	d := haversineM(from, to) * roadFactor
	distanceM = int(math.Round(d))
	durationS = int(math.Round(d / speedMPS[mode]))
	return
}

// haversineM returns the great-circle distance between two points in metres.
func haversineM(a, b point) float64 {
	const earthR = 6371000.0 // metres
	rad := math.Pi / 180
	la1, la2 := a.Lat*rad, b.Lat*rad
	dLa := (b.Lat - a.Lat) * rad
	dLo := (b.Lng - a.Lng) * rad
	h := math.Sin(dLa/2)*math.Sin(dLa/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLo/2)*math.Sin(dLo/2)
	return 2 * earthR * math.Asin(math.Min(1, math.Sqrt(h)))
}

func polyline(from, to point) string {
	return fmt.Sprintf("map-sim:v1:%.6f,%.6f->%.6f,%.6f", from.Lat, from.Lng, to.Lat, to.Lng)
}

func normMode(m string) (string, bool) {
	if m == "" {
		return "CAR", true
	}
	if _, ok := speedMPS[m]; ok {
		return m, true
	}
	return "", false
}

func validate(from, to point) string {
	for _, p := range []point{from, to} {
		if p.Lat < -90 || p.Lat > 90 {
			return "lat is out of range [-90,90]"
		}
		if p.Lng < -180 || p.Lng > 180 {
			return "lng is out of range [-180,180]"
		}
	}
	return ""
}

func qFloat(q map[string][]string, k string) float64 {
	if v, ok := q[k]; ok && len(v) > 0 {
		f, _ := strconv.ParseFloat(v[0], 64)
		return f
	}
	return 0
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, msg, field, reason string) {
	inner := map[string]any{"code": "VALIDATION", "message": msg, "trace_id": traceID(r), "retryable": false}
	if field != "" {
		inner["details"] = []map[string]string{{"field": field, "reason": reason}}
	}
	writeJSON(w, status, map[string]any{"error": inner})
}

func traceID(r *http.Request) string {
	if tp := r.Header.Get("traceparent"); len(tp) >= 35 {
		return tp[3:35]
	}
	return "000000000000000000000000map-sim0"[:32]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func selfCheck(u string) {
	resp, err := http.Get(u)
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	_ = resp.Body.Close()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
