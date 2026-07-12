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

// wallet.go is the "+ wallet" scope: a stored-value balance a customer can pay
// with. A wallet-funded authorize DEBITS the balance inside the SAME D9
// money-mutation transaction as the payment row, so a retried wallet payment
// debits exactly once. Both money paths (card via PSP, wallet via balance) share
// the one idempotency guard. Top-up (credit) is itself a D9-idempotent mutation.
//
// All in-transaction wallet mutations are CONDITIONAL WRITES (no reads inside the
// tx), so they never contend for the single in-memory SQLite writer connection:
//   - debit: UPDATE … SET balance = balance - amt WHERE balance >= amt (0 rows ⇒ insufficient)
//   - credit: UPDATE … += amt; if 0 rows, INSERT (portable UPSERT)

// debitWalletTx debits amount from a customer's wallet inside the caller's
// idempotent tx. A conditional UPDATE enforces sufficient funds atomically; 0
// rows affected ⇒ WALLET_INSUFFICIENT_FUNDS (422). It appends a wallet_entries
// ledger row (the audit trail). No read ⇒ no deadlock on the single writer.
func (pm *payments) debitWalletTx(ctx context.Context, tx idempotency.Execer, customerID string, amount money, paymentID string, now time.Time) error {
	n, err := tx.Exec(ctx,
		`UPDATE wallets SET balance_minor = balance_minor - ?, updated_at = ?
		  WHERE customer_id = ? AND currency = ? AND balance_minor >= ?`,
		amount.Amount, now, customerID, amount.Currency, amount.Amount)
	if err != nil {
		return err
	}
	if n == 0 {
		return shoperr.New(codeInsufficient, "")
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO wallet_entries (entry_id, customer_id, payment_id, delta_minor, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		newToken("wal"), customerID, paymentID, -amount.Amount, "authorize", now)
	return err
}

// --- wallet HTTP surface ----------------------------------------------------

// walletCreditRequest tops up a wallet.
type walletCreditRequest struct {
	CustomerID string `json:"customer_id"`
	Amount     *money `json:"amount"`
}

// Credit is a D9-idempotent wallet top-up: a retried credit (same Idempotency-Key)
// applies exactly once. Portable UPSERT (UPDATE-then-INSERT) keeps it a pure write
// path inside the idempotent tx.
func (pm *payments) Credit(ctx context.Context, tx idempotency.Execer, body []byte, now time.Time) (int, []byte, error) {
	var in walletCreditRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return 0, nil, shoperr.New(shoperr.CodeValidation, "request body must be valid JSON")
		}
	}
	if in.CustomerID == "" || in.Amount == nil || in.Amount.Amount <= 0 {
		return 0, nil, shoperr.New(shoperr.CodeValidation, "customer_id and a positive amount are required")
	}
	cur := in.Amount.Currency
	if cur == "" {
		cur = "THB"
	}
	n, err := tx.Exec(ctx,
		`UPDATE wallets SET balance_minor = balance_minor + ?, updated_at = ? WHERE customer_id = ? AND currency = ?`,
		in.Amount.Amount, now, in.CustomerID, cur)
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (customer_id, region, balance_minor, currency, updated_at) VALUES (?, ?, ?, ?, ?)`,
			in.CustomerID, pm.region, in.Amount.Amount, cur, now); err != nil {
			return 0, nil, err
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallet_entries (entry_id, customer_id, payment_id, delta_minor, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		newToken("wal"), in.CustomerID, "", in.Amount.Amount, "credit", now); err != nil {
		return 0, nil, err
	}
	resp, _ := json.Marshal(map[string]any{"customer_id": in.CustomerID, "credited": amountMap(*in.Amount)})
	return http.StatusOK, resp, nil
}

// walletBalance reads a wallet balance (post-commit read; connection is free).
func (s *store) walletBalance(ctx context.Context, customerID string) (money, bool, error) {
	var m money
	err := s.db.QueryRowContext(ctx,
		`SELECT balance_minor, currency FROM wallets WHERE customer_id = ?`, customerID).
		Scan(&m.Amount, &m.Currency)
	if err == sql.ErrNoRows {
		return money{Amount: 0, Currency: "THB"}, false, nil
	}
	if err != nil {
		return money{}, false, err
	}
	return m, true, nil
}
