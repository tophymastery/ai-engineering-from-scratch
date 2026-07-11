package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

// sign.go — HMAC-SHA256 sign + verify over a quote's canonical bytes (quote.go).
// This is the integrity gate D10 requires: a quote is "signed (HMAC over quote
// body + expiry) so checkout can verify integrity", and a tampered OR expired
// quote must be rejected with 422 on 100% of a fixture set (mirroring V-T1's
// 1000/1000 forgery rigor; here HMAC forgery + expiry). The two rejection
// sentinels below both map to 422 (02 §2 domain rule) in main.go.

var (
	// errQuoteInvalid: the HMAC did not verify (any economically-meaningful field
	// mutated) OR the kid is unknown/retired. A tampered/forged quote.
	errQuoteInvalid = errors.New("quote signature invalid")
	// errQuoteExpired: the signature is authentic but the signed expires_at is in
	// the past (frozen clock).
	errQuoteExpired = errors.New("quote expired")
)

// b64 encodes signatures unpadded-url; b64strict additionally rejects
// non-canonical trailing bits on DECODE, closing the same forgery hole V-T1
// documents (flipping the unused bits of the final base64 char would otherwise
// decode to the same bytes and let a tampered signature verify).
var (
	b64       = base64.RawURLEncoding
	b64strict = base64.RawURLEncoding.Strict()
)

// signQuote computes and STAMPS q.Kid + q.Signature using the keyring's current
// primary key. Signing always uses the primary (rotation: new quotes get the new
// key); verification (below) accepts any key still in the ring.
func signQuote(km *keyManager, q *Quote) error {
	kid, secret, err := km.signingKey()
	if err != nil {
		return err
	}
	q.Kid = kid
	q.Signature = b64.EncodeToString(hmacSum(secret, canonicalQuoteBytes(q)))
	return nil
}

// verifyQuote is the tamper/expiry gate. Order mirrors V-T1's Verify: resolve
// the kid (unknown ⇒ reject), constant-time-compare the HMAC (mismatch ⇒
// reject), THEN check expiry against now (frozen clock). Returns nil only for an
// authentic, unexpired quote.
//
//   - tampered amount / line item / cart_id / expiry-extension ⇒ canonical bytes
//     differ ⇒ HMAC mismatch ⇒ errQuoteInvalid (→ 422).
//   - unknown or retired kid ⇒ errQuoteInvalid (→ 422).
//   - authentic but past expires_at ⇒ errQuoteExpired (→ 422).
func verifyQuote(km *keyManager, q *Quote, now time.Time) error {
	secret, ok := km.lookup(q.Kid)
	if !ok {
		return errQuoteInvalid
	}
	got, err := b64strict.DecodeString(q.Signature)
	if err != nil {
		return errQuoteInvalid
	}
	want := hmacSum(secret, canonicalQuoteBytes(q))
	if !hmac.Equal(got, want) {
		return errQuoteInvalid
	}
	// Signature authentic — now enforce the signed expiry.
	exp, err := time.Parse(time.RFC3339, q.ExpiresAt)
	if err != nil {
		return errQuoteInvalid // a malformed expiry on an otherwise-signed quote is a tamper
	}
	if !now.Before(exp) { // now >= exp ⇒ expired
		return errQuoteExpired
	}
	return nil
}

func hmacSum(secret, msg []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(msg)
	return m.Sum(nil)
}
