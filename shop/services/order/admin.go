package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

// admin.go is the ops/admin-bff surface for V-T9: BULK-COMPENSATION APIs and the
// STUCK-ORDER console (the SLO < 0.05%/day board). These are the operator levers
// for the saga — mass-cancel a cohort (with refund/void compensation) and see
// which orders are stalled past their timer windows. The admin-bff exposes them
// as a passthrough console (render-only manifest, mirroring how prior slices
// shipped BFF passthrough — disclosed in VERIFICATION §V-T9).

// bulkCancelRequest cancels a set of orders (ops remediation cohort).
type bulkCancelRequest struct {
	OrderIDs []string `json:"order_ids"`
	Reason   string   `json:"reason"`
}

type bulkCancelResult struct {
	Requested int               `json:"requested"`
	Cancelled int               `json:"cancelled"`
	Skipped   int               `json:"skipped"`
	Errors    map[string]string `json:"errors,omitempty"`
}

// handleBulkCancel cancels each order via the SAME saga path a single cancel
// uses, so compensation (void/refund) runs per order exactly once. An order in an
// illegal state to cancel is skipped (counted), never a hard failure — bulk ops
// are best-effort over a cohort.
func (s *server) handleBulkCancel(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled(w, r) {
		return
	}
	var in bulkCancelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "request body must be valid JSON"))
		return
	}
	now := nowFor(r.Context(), s.clock)
	res := bulkCancelResult{Requested: len(in.OrderIDs), Errors: map[string]string{}}
	for _, id := range in.OrderIDs {
		// Cancel from wherever the order is: pick the legal cancel trigger for the
		// current state (PAYMENT_PENDING⇒user_cancel[void]; PAID⇒merchant_reject
		// [refund]; ACCEPTED⇒dispatch_exhausted[refund]).
		trig, ok := s.cancelTriggerFor(r.Context(), id)
		if !ok {
			res.Skipped++
			continue
		}
		_, applied, err := s.sg.ApplyTrigger(r.Context(), id, trig, map[string]any{"actor": "ops", "reason": in.Reason}, now)
		switch {
		case err != nil && isNotFound(err):
			res.Skipped++
		case err != nil && isInvalidTransition(err):
			res.Skipped++
		case err != nil:
			res.Errors[id] = err.Error()
		case applied:
			res.Cancelled++
		default:
			res.Skipped++
		}
	}
	if len(res.Errors) == 0 {
		res.Errors = nil
	}
	writeJSON(w, http.StatusOK, res)
}

// cancelTriggerFor returns the legal cancel trigger for an order's current state,
// or ok=false if it is terminal / not found / not cancellable.
func (s *server) cancelTriggerFor(ctx context.Context, orderID string) (Trigger, bool) {
	o, ok, err := s.st.getOrder(ctx, orderID)
	if err != nil || !ok {
		return "", false
	}
	switch o.Status {
	case StatePaymentPending:
		return TrigUserCancel, true
	case StatePaid:
		return TrigMerchantReject, true
	case StateAccepted:
		return TrigDispatchExhausted, true
	default:
		return "", false
	}
}

// stuckOrder is one row of the stuck-order console.
type stuckOrder struct {
	OrderID    string `json:"order_id"`
	Status     string `json:"status"`
	AgeSeconds int64  `json:"age_seconds"`
	UpdatedAt  string `json:"updated_at"`
}

// handleStuck lists orders stalled in a non-terminal state past a threshold — the
// data behind the stuck-order SLO board (< 0.05%/day) + alert. `older_than` is a
// Go duration (default 15m).
func (s *server) handleStuck(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled(w, r) {
		return
	}
	older := DefaultRemediationWindow
	if q := r.URL.Query().Get("older_than"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			older = d
		}
	}
	now := nowFor(r.Context(), s.clock)
	cutoff := now.Add(-older)
	rows, err := s.st.db.QueryContext(r.Context(),
		`SELECT order_id, status, updated_at FROM orders
		  WHERE status NOT IN ('SETTLED','CANCELLED') AND updated_at <= ?
		  ORDER BY updated_at ASC`, cutoff)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	defer rows.Close()
	out := []stuckOrder{}
	for rows.Next() {
		var id, status string
		var upd time.Time
		if err := rows.Scan(&id, &status, &upd); err != nil {
			s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
			return
		}
		out = append(out, stuckOrder{OrderID: id, Status: status, AgeSeconds: int64(now.Sub(upd).Seconds()), UpdatedAt: upd.UTC().Format(time.RFC3339)})
	}
	total, _ := s.st.orderCount(r.Context())
	ratio := 0.0
	if total > 0 {
		ratio = float64(len(out)) / float64(total)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stuck": out, "stuck_count": len(out), "total_orders": total,
		"stuck_ratio": ratio, "slo_threshold": 0.0005, // < 0.05%/day
	})
}

// handleSweep fires all currently-due durable timers immediately (an ops lever +
// the E2E/test hook to advance timers deterministically without waiting for the
// tick). Honours X-Test-Clock so an operator/E2E can fire remediation on a frozen
// clock. Returns the number fired.
func (s *server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled(w, r) {
		return
	}
	// Use the request clock (X-Test-Clock in non-prod) so E2E can advance to due.
	now := nowFor(r.Context(), s.clock)
	sw := NewSweeper(s.st, "order-sweeper-manual", clockAt{now}, func(ctx context.Context, t TimerRow) error {
		_, _, err := s.sg.ApplyTrigger(ctx, t.OrderID, t.Trigger, map[string]any{"timer": t.Kind}, now)
		if err != nil && !isInvalidTransition(err) && !isNotFound(err) {
			return err
		}
		return nil
	})
	fired, err := sw.SweepOnce(r.Context())
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fired": fired})
}

// clockAt is a fixed Clock at a chosen instant (for the manual sweep using the
// request's effective time).
type clockAt struct{ t time.Time }

func (c clockAt) Now() time.Time { return c.t }

// adminEnabled gates the ops endpoints: enabled outside prod AND requires saga_v1.
func (s *server) adminEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.admin {
		s.fail(w, r, shoperr.New(shoperr.CodeForbidden, "admin endpoints are disabled in this build"))
		return false
	}
	if !s.sagaEnabled(r) {
		s.fail(w, r, shoperr.New(codeSagaDisabled, ""))
		return false
	}
	return true
}

var _ = context.Background
