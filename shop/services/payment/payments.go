package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/idempotency"
)

// payments.go is the D9 money-mutation core + HTTP surface. authorize / capture
// / refund are EACH a money mutation guarded by the D9 transaction-durable
// idempotency lib: the UNIQUE(idempotency_key) insert, the payment write, and
// the payment.* outbox event commit in ONE transaction. A retried mutation (same
// Idempotency-Key) is exactly-once at the DB level — one charge, one capture, one
// refund — proven by row/charge counts under -race. The advisory Redis cache is
// only a replay accelerator; the PG UNIQUE constraint is the source of truth, so
// dropping the cache mid-storm cannot double-charge (the failover invariant).

var (
	codePaymentNotFound = shoperr.Register("PAYMENT_NOT_FOUND", 404, false, "No payment exists with that id.")
	codePaymentDeclined = shoperr.Register("PAYMENT_DECLINED", 402, false, "The issuer declined the card.")
	codePSPTimeout      = shoperr.Register("PAYMENT_PSP_TIMEOUT", 504, true, "The payment provider did not respond in time; retry.")
	codePSPUnavailable  = shoperr.Register("PAYMENT_PSP_UNAVAILABLE", 503, true, "The payment provider is unavailable; retry.")
	codeInsufficient    = shoperr.Register("WALLET_INSUFFICIENT_FUNDS", 422, false, "The wallet balance is insufficient for this amount.")
	codeOrderConflict   = shoperr.Register("PAYMENT_ORDER_CONFLICT", 409, false, "This order already has a payment; a second authorization is not allowed.")
)

// payments carries the store + resilient PSP adapter for the money mutations.
type payments struct {
	st     *store
	psp    PSP
	region string
	// callbackURL is where the PSP posts async webhooks (the payment service's own
	// /v1/psp/webhooks). Empty in unit tests (webhooks injected directly).
	callbackURL string
}

func newPayments(st *store, psp PSP, region, callbackURL string) *payments {
	return &payments{st: st, psp: psp, region: region, callbackURL: callbackURL}
}

// paymentView is the Payment response body (payment.v1.yaml Payment schema:
// payment_id, order_id, status, amount, authorized_at all required).
type paymentView struct {
	PaymentID    string `json:"payment_id"`
	OrderID      string `json:"order_id"`
	Status       string `json:"status"`
	Amount       money  `json:"amount"`
	AuthorizedAt string `json:"authorized_at"`
	CaptureID    string `json:"capture_id,omitempty"`
	RefundID     string `json:"refund_id,omitempty"`
	Method       string `json:"method,omitempty"`
}

func toView(p PaymentRow) paymentView {
	return paymentView{
		PaymentID: p.PaymentID, OrderID: p.OrderID, Status: string(p.Status),
		Amount: p.Amount, AuthorizedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		CaptureID: p.CaptureID, RefundID: p.RefundID, Method: p.Method,
	}
}

// --- authorize (POST /v1/payments:authorize) --------------------------------

// authorizeRequest is the body of POST /v1/payments:authorize (02 §4.1 + payment.v1).
type authorizeRequest struct {
	OrderID         string `json:"order_id"`
	CustomerID      string `json:"customer_id"`
	PaymentMethodID string `json:"payment_method_id"`
	CardNumber      string `json:"card_number"` // sandbox: the PSP-fixture card (…0002 declines, …0044 times out)
	Method          string `json:"method"`      // card | wallet
	Amount          *money `json:"amount"`
}

// Authorize is the D9 flagship money mutation. The idempotency lib runs the
// authorize effect exactly once for an Idempotency-Key: the PSP charge (or wallet
// debit), the payment row, the payment.authorized/failed outbox event, and the
// idempotency key all commit atomically. Two concurrent same-key authorizes ⇒
// one wins the UNIQUE insert and charges once; the losers replay its response —
// never a second charge. A UNIQUE(order_id) index is a second, order-level guard.
func (pm *payments) Authorize(ctx context.Context, tx idempotency.Execer, body []byte, now time.Time) (int, []byte, error) {
	var in authorizeRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return 0, nil, shoperr.New(shoperr.CodeValidation, "request body must be valid JSON")
		}
	}
	if in.OrderID == "" {
		return 0, nil, shoperr.New(shoperr.CodeValidation, "order_id is required",
			shoperr.Detail{Field: "order_id", Reason: "required"})
	}
	amount := money{Amount: 42550, Currency: "THB"}
	if in.Amount != nil {
		amount = *in.Amount
	}
	method := in.Method
	if method == "" {
		method = "card"
	}

	p := PaymentRow{
		PaymentID:  newToken("pay"),
		OrderID:    in.OrderID,
		CustomerID: orDefault(in.CustomerID, "usr_anon"),
		Region:     pm.region,
		Method:     method,
		Amount:     amount,
		Status:     StateAuthorized,
		PSP:        "payment-sim",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// CLAIM the order BEFORE the PSP charge (insert-first). UNIQUE(order_id) is the
	// order-level guard: if a different Idempotency-Key already authorized this
	// order, this insert fails and we return 409 BEFORE calling the PSP — so a
	// same-order double-submit can never produce a second charge. (A same-KEY
	// retry never reaches here: it blocks on the idempotency-key UNIQUE and replays.)
	// A crash/timeout after the claim rolls the whole tx back, releasing the claim.
	if err := pm.st.insertPaymentTx(ctx, tx, p); err != nil {
		if pm.st.dialect.IsUniqueViolation(err) {
			return 0, nil, shoperr.New(codeOrderConflict, "")
		}
		return 0, nil, err
	}

	if method == "wallet" {
		// Wallet-funded: debit the stored-value balance IN THIS TX (no PSP). A
		// retried wallet payment debits exactly once (D9).
		if err := pm.debitWalletTx(ctx, tx, p.CustomerID, amount, p.PaymentID, now); err != nil {
			return 0, nil, err // WALLET_INSUFFICIENT_FUNDS (422) — rolls the claim back
		}
		p.AuthID = "wallet_" + p.PaymentID
	} else {
		// Card: charge the PSP. The order claim is already held, so at most one
		// charge per order can commit; the same-key retry replays, never re-charges.
		card := orDefault(in.CardNumber, "4111111111111111")
		res, err := pm.psp.Authorize(ctx, in.OrderID, card, amount, pm.callbackURL)
		if err != nil {
			if declined(err) {
				// Record the decline durably (audit + order compensation) and emit
				// payment.failed. Return 402 with err=nil so the record COMMITS and
				// same-key retries replay the decline (no re-charge). The order claim
				// stays (a DECLINED row), so a re-submit under a new key ⇒ 409, not a
				// second PSP attempt.
				p.Status = StateDeclined
				if _, e := tx.Exec(ctx, `UPDATE payments SET status=? WHERE payment_id=?`, string(StateDeclined), p.PaymentID); e != nil {
					return 0, nil, e
				}
				if e := pm.st.appendEventTx(ctx, tx, p.PaymentID, "authorize", "", StateDeclined, map[string]any{"reason": "card_declined"}, now); e != nil {
					return 0, nil, e
				}
				env, e := buildEnvelope("payment.failed", p, failedPayload(p, "card_declined", now), now)
				if e != nil {
					return 0, nil, e
				}
				if e := pm.st.stageEventTx(ctx, tx, "payment.failed", env); e != nil {
					return 0, nil, e
				}
				return http.StatusPaymentRequired, mustJSON(toView(p)), nil
			}
			// Timeout / unavailable: roll the whole tx back (releasing the order
			// claim; we never confirmed a charge) and surface a retryable error —
			// the circuit breaker + retry live in the resilient PSP wrapper.
			return 0, nil, mapPSPError(err)
		}
		p.AuthID = res.AuthID
	}

	if _, err := tx.Exec(ctx, `UPDATE payments SET auth_id=? WHERE payment_id=?`, p.AuthID, p.PaymentID); err != nil {
		return 0, nil, err
	}
	if err := pm.st.appendEventTx(ctx, tx, p.PaymentID, "authorize", "", StateAuthorized, map[string]any{"method": method, "auth_id": p.AuthID}, now); err != nil {
		return 0, nil, err
	}
	env, err := buildEnvelope("payment.authorized", p, authorizedPayload(p), now)
	if err != nil {
		return 0, nil, err
	}
	if err := pm.st.stageEventTx(ctx, tx, "payment.authorized", env); err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, mustJSON(toView(p)), nil
}

// --- capture / refund via the D9 idempotency lib (API path) -----------------
//
// The customer/BFF capture + refund endpoints run their money mutation INSIDE
// the idempotency lib's transaction (idempotency.Execer), so the guarded status
// UPDATE, the PSP call, the capture_id/refund_id write, the payment_events row,
// the payment.* outbox event, and the UNIQUE(idempotency_key) insert all commit
// atomically (D9). A retried capture/refund (same Idempotency-Key) is
// exactly-once at the DB level — proven by capture/refund counts under -race.
// The payment is pre-read OUTSIDE the tx (for auth_id/capture_id/amount); the
// GUARDED UPDATE (WHERE status=<from>) is the real concurrency guard, so a stale
// pre-read can never double-apply.

// captureExec captures an AUTHORIZED payment inside the idempotent tx.
func (pm *payments) captureExec(ctx context.Context, tx idempotency.Execer, pre PaymentRow, source string, now time.Time) (int, []byte, error) {
	n, err := tx.Exec(ctx,
		`UPDATE payments SET status=?, updated_at=? WHERE payment_id=? AND status=?`,
		string(StateCaptured), now, pre.PaymentID, string(StateAuthorized))
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		if pre.Status == StateCaptured {
			return http.StatusOK, mustJSON(toView(pre)), nil // idempotent no-op
		}
		return 0, nil, shoperr.New(codeInvalidTransition, "",
			shoperr.Detail{Field: "status", Reason: "capture requires AUTHORIZED, is " + string(pre.Status)})
	}
	capID, err := pm.psp.Capture(ctx, pre.AuthID, pre.Amount, pm.callbackURL)
	if err != nil {
		return 0, nil, mapPSPError(err) // rollback → retryable
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET capture_id=? WHERE payment_id=?`, capID, pre.PaymentID); err != nil {
		return 0, nil, err
	}
	p := pre
	p.Status, p.CaptureID = StateCaptured, capID
	if err := pm.st.appendEventTx(ctx, tx, p.PaymentID, Trigger("capture:"+source), StateAuthorized, StateCaptured, map[string]any{"capture_id": capID}, now); err != nil {
		return 0, nil, err
	}
	env, err := buildEnvelope("payment.captured", p, capturedPayload(p, now), now)
	if err != nil {
		return 0, nil, err
	}
	if err := pm.st.stageEventTx(ctx, tx, "payment.captured", env); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, mustJSON(toView(p)), nil
}

// refundExec refunds a CAPTURED payment inside the idempotent tx.
func (pm *payments) refundExec(ctx context.Context, tx idempotency.Execer, pre PaymentRow, source string, now time.Time) (int, []byte, error) {
	n, err := tx.Exec(ctx,
		`UPDATE payments SET status=?, updated_at=? WHERE payment_id=? AND status=?`,
		string(StateRefunded), now, pre.PaymentID, string(StateCaptured))
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		if pre.Status == StateRefunded {
			return http.StatusOK, mustJSON(toView(pre)), nil // idempotent no-op
		}
		return 0, nil, shoperr.New(codeInvalidTransition, "",
			shoperr.Detail{Field: "status", Reason: "refund requires CAPTURED, is " + string(pre.Status)})
	}
	refID, err := pm.psp.Refund(ctx, pre.CaptureID, pre.Amount, pm.callbackURL)
	if err != nil {
		return 0, nil, mapPSPError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET refund_id=? WHERE payment_id=?`, refID, pre.PaymentID); err != nil {
		return 0, nil, err
	}
	p := pre
	p.Status, p.RefundID = StateRefunded, refID
	if err := pm.st.appendEventTx(ctx, tx, p.PaymentID, Trigger("refund:"+source), StateCaptured, StateRefunded, map[string]any{"refund_id": refID}, now); err != nil {
		return 0, nil, err
	}
	env, err := buildEnvelope("payment.refunded", p, refundedPayload(p, now), now)
	if err != nil {
		return 0, nil, err
	}
	if err := pm.st.stageEventTx(ctx, tx, "payment.refunded", env); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, mustJSON(toView(p)), nil
}

// --- capture / refund / void money-mutation core (raw tx) -------------------

// captureInTx captures an AUTHORIZED payment: guarded transition → PSP capture →
// record capture_id + payment.captured. Idempotent: an already-CAPTURED payment
// is a no-op (returns it unchanged, no second PSP capture).
func (pm *payments) captureInTx(ctx context.Context, tx *sql.Tx, p PaymentRow, source string, now time.Time) (PaymentRow, error) {
	if p.Status == StateCaptured {
		return p, nil // already captured — idempotent no-op
	}
	to, err := Transition(p.Status, TrigCapture)
	if err != nil {
		return p, err // 409 PAYMENT_INVALID_TRANSITION
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE payments SET status=?, updated_at=? WHERE payment_id=? AND status=?`,
		string(to), now, p.PaymentID, string(p.Status))
	if err != nil {
		return p, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return p, nil // lost race — another tx already captured
	}
	capID, err := pm.psp.Capture(ctx, p.AuthID, p.Amount, pm.callbackURL)
	if err != nil {
		return p, mapPSPError(err) // rollback — retry/circuit handles it
	}
	if _, err := tx.ExecContext(ctx, `UPDATE payments SET capture_id=? WHERE payment_id=?`, capID, p.PaymentID); err != nil {
		return p, err
	}
	p.Status, p.CaptureID = to, capID
	if err := pm.st.appendEventSQLTx(ctx, tx, p.PaymentID, "capture:"+source, StateAuthorized, StateCaptured, map[string]any{"capture_id": capID}, now); err != nil {
		return p, err
	}
	env, err := buildEnvelope("payment.captured", p, capturedPayload(p, now), now)
	if err != nil {
		return p, err
	}
	return p, pm.st.ob.WriteInTx(ctx, tx, "payment.captured", env)
}

// refundInTx refunds a CAPTURED payment: guarded transition → PSP refund → record
// refund_id + payment.refunded. Idempotent: an already-REFUNDED payment is a no-op.
func (pm *payments) refundInTx(ctx context.Context, tx *sql.Tx, p PaymentRow, source string, now time.Time) (PaymentRow, error) {
	if p.Status == StateRefunded {
		return p, nil // already refunded — idempotent no-op
	}
	to, err := Transition(p.Status, TrigRefund)
	if err != nil {
		return p, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE payments SET status=?, updated_at=? WHERE payment_id=? AND status=?`,
		string(to), now, p.PaymentID, string(p.Status))
	if err != nil {
		return p, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return p, nil
	}
	refID, err := pm.psp.Refund(ctx, p.CaptureID, p.Amount, pm.callbackURL)
	if err != nil {
		return p, mapPSPError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE payments SET refund_id=? WHERE payment_id=?`, refID, p.PaymentID); err != nil {
		return p, err
	}
	p.Status, p.RefundID = to, refID
	if err := pm.st.appendEventSQLTx(ctx, tx, p.PaymentID, "refund:"+source, StateCaptured, StateRefunded, map[string]any{"refund_id": refID}, now); err != nil {
		return p, err
	}
	env, err := buildEnvelope("payment.refunded", p, refundedPayload(p, now), now)
	if err != nil {
		return p, err
	}
	return p, pm.st.ob.WriteInTx(ctx, tx, "payment.refunded", env)
}

// voidInTx releases an uncaptured AUTHORIZED hold (the pre-capture compensation:
// order.cancelled ⇒ void). payment-sim has no explicit void, so the hold is
// released locally + recorded; no PSP call, no captured funds to reverse.
func (pm *payments) voidInTx(ctx context.Context, tx *sql.Tx, p PaymentRow, source string, now time.Time) (PaymentRow, error) {
	if p.Status == StateVoided {
		return p, nil
	}
	to, err := Transition(p.Status, TrigVoid)
	if err != nil {
		return p, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE payments SET status=?, updated_at=? WHERE payment_id=? AND status=?`,
		string(to), now, p.PaymentID, string(p.Status))
	if err != nil {
		return p, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return p, nil
	}
	p.Status = to
	return p, pm.st.appendEventSQLTx(ctx, tx, p.PaymentID, "void:"+source, StateAuthorized, StateVoided, map[string]any{"reason": source}, now)
}

// --- tx read helpers --------------------------------------------------------

// lockPaymentTx re-reads a payment inside a raw tx (the capture/refund API path).
func (pm *payments) lockPaymentTx(ctx context.Context, tx *sql.Tx, paymentID string) (PaymentRow, bool, error) {
	return pm.st.scanPayment(tx.QueryRowContext(ctx, paymentSelect+` WHERE payment_id = ?`, paymentID))
}

// lockPaymentByOrderTx re-reads a payment by order_id inside a raw tx.
func (pm *payments) lockPaymentByOrderTx(ctx context.Context, tx *sql.Tx, orderID string) (PaymentRow, bool, error) {
	return pm.st.scanPayment(tx.QueryRowContext(ctx, paymentSelect+` WHERE order_id = ?`, orderID))
}

// --- helpers ----------------------------------------------------------------

// mapPSPError maps a typed PSP error to the 02 §2 wire error.
func mapPSPError(err error) error {
	pe, ok := err.(*pspError)
	if !ok {
		return shoperr.New(shoperr.CodeInternal, err.Error())
	}
	switch pe.Kind {
	case pspDeclined:
		return shoperr.New(codePaymentDeclined, "")
	case pspTimeout:
		return shoperr.New(codePSPTimeout, "")
	default:
		return shoperr.New(codePSPUnavailable, "")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// captureRefundVia runs a capture or refund from a raw tx started here (the API
// handlers). It re-reads the payment under the tx, applies the mutation, and
// returns the updated row. Returns 404 if the payment is unknown.
func (pm *payments) captureRefundVia(ctx context.Context, paymentID string, refund bool, source string, now time.Time) (PaymentRow, int, error) {
	tx, err := pm.st.db.BeginTx(ctx, nil)
	if err != nil {
		return PaymentRow{}, 0, err
	}
	p, ok, err := pm.lockPaymentTx(ctx, tx, paymentID)
	if err != nil {
		_ = tx.Rollback()
		return PaymentRow{}, 0, err
	}
	if !ok {
		_ = tx.Rollback()
		return PaymentRow{}, 0, shoperr.New(codePaymentNotFound, "")
	}
	if refund {
		p, err = pm.refundInTx(ctx, tx, p, source, now)
	} else {
		p, err = pm.captureInTx(ctx, tx, p, source, now)
	}
	if err != nil {
		_ = tx.Rollback()
		return PaymentRow{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return PaymentRow{}, 0, err
	}
	pm.st.ob.Signal()
	return p, http.StatusOK, nil
}
