package main

import (
	"encoding/json"
	"net/http"

	shoperr "github.com/shop-platform/shop/libs/errors"
)

// admin.go is the ops/admin-bff surface for V-T10: the REFUND CONSOLE and a
// payments board. Refunds initiated by an operator go through the SAME D9
// money-mutation core a customer refund uses (captureRefundVia → refundInTx), so
// compensation runs exactly once and emits payment.refunded. The admin-bff
// exposes these as a render-only passthrough console (mirroring how prior slices
// shipped BFF passthrough — disclosed in VERIFICATION §V-T10).

// handleAdminRefund refunds a captured payment on operator request
// (POST /v1/admin/payments/{id}:refund). Idempotent: a re-refund is a no-op.
func (s *server) handleAdminRefund(w http.ResponseWriter, r *http.Request, paymentID string) {
	if !s.adminEnabled(w, r) {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	now := nowFor(r.Context(), s.clock)
	source := "admin"
	if body.Reason != "" {
		source = "admin:" + body.Reason
	}
	p, code, err := s.pm.captureRefundVia(r.Context(), paymentID, true, source, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, code, toView(p))
}

// handleAdminPayments lists payments by status (the refund-console board data).
func (s *server) handleAdminPayments(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled(w, r) {
		return
	}
	status := r.URL.Query().Get("status")
	q := paymentSelect + ` ORDER BY updated_at DESC LIMIT 200`
	args := []any{}
	if status != "" {
		q = paymentSelect + ` WHERE status = ? ORDER BY updated_at DESC LIMIT 200`
		args = append(args, status)
	}
	rows, err := s.st.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.fail(w, r, shoperr.New(shoperr.CodeInternal, err.Error()))
		return
	}
	defer rows.Close()
	out := []paymentView{}
	for rows.Next() {
		p, _, e := s.st.scanPayment(rows)
		if e != nil {
			s.fail(w, r, shoperr.New(shoperr.CodeInternal, e.Error()))
			return
		}
		out = append(out, toView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"payments": out, "count": len(out)})
}

// adminEnabled gates the ops endpoints: enabled outside prod AND requires payment_v1.
func (s *server) adminEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.admin {
		s.fail(w, r, shoperr.New(shoperr.CodeForbidden, "admin endpoints are disabled in this build"))
		return false
	}
	if !s.paymentEnabled(r) {
		s.fail(w, r, shoperr.New(codePaymentDisabled, ""))
		return false
	}
	return true
}
