package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/eventbus"
)

// webhooks.go consumes the payment-sim PSP webhooks (payment.authorized /
// payment.captured / payment.refunded — S-T7). Each webhook CONFIRMS the money
// state the synchronous PSP call already recorded, and is applied EXACTLY-ONCE
// via the durable SQL inbox keyed on the webhook's own event_id. Replaying the
// SAME webhook (same event_id) 10× therefore produces exactly ONE state
// transition (one payment_events append, one webhook_state change) — the
// dedupe is a real DB UNIQUE constraint in the confirming transaction, not a
// mock. This is the "webhook 10× replay ⇒ single state transition" property.

// webhookBody is the payment-sim webhook shape.
type webhookBody struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"` // payment.authorized | payment.captured | payment.refunded
	AuthID    string `json:"auth_id"`
	CaptureID string `json:"capture_id"`
	RefundID  string `json:"refund_id"`
}

// webhookConsumer applies PSP webhooks with exactly-once effect.
type webhookConsumer struct {
	st    *store
	clock Clock
}

func newWebhookConsumer(st *store, clock Clock) *webhookConsumer {
	return &webhookConsumer{st: st, clock: clock}
}

// Apply runs one webhook through the inbox (exactly-once). Returns (applied,
// error): applied=false when the event_id was already processed (dedupe) OR the
// referenced payment is unknown. The confirmation (the state transition) runs on
// the inbox tx, so the payment_events append + the dedupe row commit atomically.
func (c *webhookConsumer) Apply(ctx context.Context, w webhookBody) (bool, error) {
	if w.EventID == "" || w.EventType == "" {
		return false, nil
	}
	env, err := eventbus.NewEnvelope(w.EventID, w.EventType, "trace_webhook",
		eventbus.Aggregate{Type: "psp", ID: firstNonEmpty(w.AuthID, w.CaptureID, w.RefundID)}, 1,
		map[string]any{"auth_id": w.AuthID, "capture_id": w.CaptureID, "refund_id": w.RefundID}, c.clock.Now())
	if err != nil {
		return false, err
	}
	msg, err := eventbus.NewMessage(w.EventType, env)
	if err != nil {
		return false, err
	}
	now := c.clock.Now()

	var confirmed bool
	inserted, err := c.st.inbx.Process(ctx, msg, func(ctx context.Context, tx *sql.Tx) error {
		// Locate the payment this webhook confirms (read via the inbox tx's own
		// connection — no separate connection, so no single-writer deadlock).
		p, ok, e := c.locateTx(ctx, tx, w)
		if e != nil {
			return e
		}
		if !ok {
			return nil // unknown reference — commit the dedupe row as a no-op
		}
		// The confirmation IS the single state transition: set webhook_state once
		// and append exactly one payment_events row.
		wstate := webhookState(w.EventType)
		if _, e := tx.ExecContext(ctx,
			`UPDATE payments SET webhook_state=?, updated_at=? WHERE payment_id=?`,
			wstate, now, p.PaymentID); e != nil {
			return e
		}
		if e := c.st.appendEventSQLTx(ctx, tx, p.PaymentID, "webhook:"+w.EventType, p.Status, p.Status,
			map[string]any{"webhook_state": wstate, "event_id": w.EventID}, now); e != nil {
			return e
		}
		confirmed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted && confirmed, nil
}

// locateTx finds the payment a webhook refers to, by the id present on the event.
func (c *webhookConsumer) locateTx(ctx context.Context, tx *sql.Tx, w webhookBody) (PaymentRow, bool, error) {
	switch {
	case w.RefundID != "":
		return c.st.scanPayment(tx.QueryRowContext(ctx, paymentSelect+` WHERE refund_id = ?`, w.RefundID))
	case w.CaptureID != "":
		return c.st.scanPayment(tx.QueryRowContext(ctx, paymentSelect+` WHERE capture_id = ?`, w.CaptureID))
	case w.AuthID != "":
		return c.st.scanPayment(tx.QueryRowContext(ctx, paymentSelect+` WHERE auth_id = ?`, w.AuthID))
	default:
		return PaymentRow{}, false, nil
	}
}

func webhookState(eventType string) string {
	switch eventType {
	case "payment.authorized":
		return "CONFIRMED_AUTHORIZED"
	case "payment.captured":
		return "CONFIRMED_CAPTURED"
	case "payment.refunded":
		return "CONFIRMED_REFUNDED"
	default:
		return "CONFIRMED"
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- HTTP endpoint ----------------------------------------------------------

// handleWebhook receives a PSP webhook (POST /v1/psp/webhooks). It always acks
// 200 (so the PSP does not retry-storm); dedupe/idempotency is by the webhook's
// own event_id in the inbox, NOT an Idempotency-Key header.
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var in webhookBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeValidation, "request body must be a valid webhook event"))
		return
	}
	applied, err := s.webhooks.Apply(r.Context(), in)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied, "event_type": in.EventType})
}
