package errors

import (
	"encoding/json"
	"net/http"
)

// TraceIDFunc extracts the live trace_id from a request's context. libs/logging
// wires libs/otel's accessor in here so the envelope's trace_id is the real
// trace (02 §2: "user report → exact trace in one hop") without errors having
// to depend on otel.
type TraceIDFunc func(*http.Request) string

// Write serialises err as the 02 §2 envelope to w, using the registered HTTP
// status for the code. traceID is embedded so the client can pivot to the trace.
func Write(w http.ResponseWriter, err error, traceID string) {
	status, env := ToEnvelope(err, traceID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// WriteRequest is Write with the trace_id pulled from the request via traceID.
// If traceID is nil, the trace_id field is empty.
func WriteRequest(w http.ResponseWriter, r *http.Request, err error, traceID TraceIDFunc) {
	tid := ""
	if traceID != nil {
		tid = traceID(r)
	}
	Write(w, err, tid)
}
