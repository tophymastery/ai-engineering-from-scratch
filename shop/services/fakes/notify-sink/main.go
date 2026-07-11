// Command notify-sink is the S-T7 queryable notification sink fake (03 §5). It
// captures push/SMS/email payloads into an in-memory, insertion-ordered inbox
// that E2E tests read back to assert what a user "received" — nothing is ever
// actually sent. Std-lib only.
//
// Endpoints (02 §1 canonical /v1 + bare task aliases):
//
//	POST   /v1/send   (alias /send)    {channel, recipient, template, subject, body}
//	GET    /v1/inbox  (alias /inbox)   ?recipient=&channel=
//	DELETE /v1/inbox  (alias /inbox)   ?recipient=   (omit to clear all)
//	GET    /healthz
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// message is a captured notification (snake_case wire fields per 02 §1).
type message struct {
	MessageID  string `json:"message_id"`
	Channel    string `json:"channel"`
	Recipient  string `json:"recipient"`
	Template   string `json:"template,omitempty"`
	Subject    string `json:"subject,omitempty"`
	Body       string `json:"body,omitempty"`
	CapturedAt string `json:"captured_at"`
}

// sink is the in-memory inbox. A monotonic counter gives stable message ids and
// a deterministic clock (fixed t0 + step) stamps captured_at so tests never read
// wall time.
type sink struct {
	mu    sync.Mutex
	msgs  []message
	seq   int64
	clock time.Time
}

func newSink() *sink {
	return &sink{clock: time.Date(2026, 7, 11, 2, 15, 0, 0, time.UTC)}
}

var validChannel = map[string]bool{"PUSH": true, "SMS": true, "EMAIL": true}

func (s *sink) send(in message) (message, string) {
	if !validChannel[in.Channel] {
		return message{}, "channel must be one of PUSH, SMS, EMAIL"
	}
	if in.Recipient == "" {
		return message{}, "recipient is required"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	in.MessageID = fmt.Sprintf("msg_%016d", s.seq)
	in.CapturedAt = s.clock.UTC().Format(time.RFC3339)
	s.clock = s.clock.Add(time.Second)
	s.msgs = append(s.msgs, in)
	return in, ""
}

func (s *sink) inbox(recipient, channel string) []message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []message{}
	for _, m := range s.msgs {
		if recipient != "" && m.Recipient != recipient {
			continue
		}
		if channel != "" && m.Channel != channel {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *sink) clear(recipient string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if recipient == "" {
		n := len(s.msgs)
		s.msgs = nil
		return n
	}
	kept := s.msgs[:0:0]
	cleared := 0
	for _, m := range s.msgs {
		if m.Recipient == recipient {
			cleared++
			continue
		}
		kept = append(kept, m)
	}
	s.msgs = kept
	return cleared
}

func main() {
	port := envOr("PORT", "8093")
	hc := flag.Bool("healthcheck", false, "probe own /healthz and exit 0/1 (container healthcheck)")
	flag.Parse()
	if *hc {
		selfCheck("http://localhost:" + port + "/healthz")
		return
	}
	addr := ":" + port
	log.Printf("notify-sink on %s", addr)
	if err := http.ListenAndServe(addr, NewMux(newSink())); err != nil {
		log.Fatalf("notify-sink exited: %v", err)
	}
}

// NewMux wires the inbox HTTP surface for a sink (exported for tests).
func NewMux(s *sink) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "notify-sink"})
	})

	send := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed", "", "")
			return
		}
		var in message
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, r, http.StatusBadRequest, "body must be valid JSON", "body", "invalid_json")
			return
		}
		m, e := s.send(in)
		if e != "" {
			writeErr(w, r, http.StatusBadRequest, e, "channel", "invalid_enum")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"message_id": m.MessageID, "channel": m.Channel, "recipient": m.Recipient, "captured_at": m.CapturedAt,
		})
	}

	inbox := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.Method {
		case http.MethodGet:
			msgs := s.inbox(q.Get("recipient"), q.Get("channel"))
			writeJSON(w, http.StatusOK, map[string]any{"count": len(msgs), "messages": msgs})
		case http.MethodDelete:
			writeJSON(w, http.StatusOK, map[string]any{"cleared": s.clear(q.Get("recipient"))})
		default:
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed", "", "")
		}
	}

	for _, base := range []string{"/v1", ""} {
		mux.HandleFunc(base+"/send", send)
		mux.HandleFunc(base+"/inbox", inbox)
	}
	return mux
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
	return "00000000000000000000000notify-sink"[:32]
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
