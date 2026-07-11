package idempotency

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

// Wire header names (02 §3).
const (
	KeyHeader      = "Idempotency-Key"
	ReplayedHeader = "Idempotency-Replayed"
)

// HTTPBusiness is the caller's effect for an idempotent HTTP mutation. It runs
// inside the durable transaction (D9) and returns the response to persist and
// replay. r.Body has already been buffered; read the provided body slice.
type HTTPBusiness func(ctx context.Context, tx Execer, body []byte) (code int, respBody []byte, err error)

// TraceIDFunc extracts the live trace_id for the error envelope (wired from
// libs/otel by the service).
type TraceIDFunc func(*http.Request) string

// HTTP implements the 02 §3 idempotency wire protocol for one mutating request:
//
//   - missing Idempotency-Key ⇒ 400 IDEMPOTENCY_KEY_REQUIRED
//   - fresh key ⇒ run business once in the durable txn; persist + return its
//     response
//   - same key + same body ⇒ replay stored response with Idempotency-Replayed: true
//   - same key + different body ⇒ 409 IDEMPOTENCY_KEY_REUSED
//   - concurrent duplicate still settling ⇒ 409 IDEMPOTENCY_IN_PROGRESS + Retry-After
//
// It writes the full response (including replay/error envelopes) to w.
func (m *Manager) HTTP(w http.ResponseWriter, r *http.Request, traceID TraceIDFunc, business HTTPBusiness) {
	tid := ""
	if traceID != nil {
		tid = traceID(r)
	}
	key := r.Header.Get(KeyHeader)
	if key == "" {
		shoperr.Write(w, shoperr.New(shoperr.CodeIdempotencyKeyRequired, ""), tid)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		shoperr.Write(w, shoperr.New(shoperr.CodeValidation, "could not read request body"), tid)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body)) // allow business to re-read if needed
	reqHash := RequestHash(r.Method, routePattern(r), body)

	out, doErr := m.Do(r.Context(), key, reqHash, func(ctx context.Context, tx Execer) (int, []byte, error) {
		return business(ctx, tx, body)
	})
	if doErr != nil {
		// Advise a Retry-After on the retryable in-progress case.
		var pe *shoperr.Error
		if asShopErr(doErr, &pe) && pe.Code == shoperr.CodeIdempotencyInProgress {
			w.Header().Set("Retry-After", strconv.Itoa(int(m.InProgressRetryAfter/time.Second)+1))
		}
		shoperr.Write(w, doErr, tid)
		return
	}
	if out.Replayed {
		w.Header().Set(ReplayedHeader, "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Code)
	_, _ = w.Write(out.Body)
}

func routePattern(r *http.Request) string {
	if p := r.Header.Get("X-Route-Pattern"); p != "" {
		return p
	}
	return r.URL.Path
}

func asShopErr(err error, target **shoperr.Error) bool {
	for err != nil {
		if pe, ok := err.(*shoperr.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
