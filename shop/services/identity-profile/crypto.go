package main

// crypto.go — envelope encryption + crypto-shredding, the mechanism D3 mandates.
//
// D3: "Erasure = destroy the per-user data-encryption key (crypto-shredding),
// making immutable events/backups unreadable." The scheme is standard envelope
// encryption:
//
//   - A single master KEK (key-encryption key) lives in the process / KMS.
//   - Every user gets a random 256-bit DEK (data-encryption key). The DEK is
//     stored ONLY once, WRAPPED by the KEK, in the per-cell keystore
//     (data_keys). It is never written next to the ciphertext and never leaves
//     the owning cell.
//   - Each PII field is sealed with AES-256-GCM under the user's DEK, with the
//     user token as additional-authenticated-data (so a ciphertext can't be
//     replayed under another user's row).
//
// Crypto-shredding erasure = destroy the wrapped DEK row (and any backup of it).
// The KEK staying intact does not help: with the only copy of the DEK gone, the
// PII ciphertext — wherever it still physically sits (primary store, replica,
// immutable backup, an event that snapshotted a field before the token-only
// rule, a WAL segment) — is unrecoverable. That is why erasure against an
// append-only substrate is even possible.
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// errKeyDestroyed is returned by any decrypt whose DEK has been crypto-shredded.
// It is the observable proof that erasure made the PII unreadable.
var errKeyDestroyed = errors.New("data-encryption key destroyed (crypto-shredded) — PII unrecoverable")

const dekLen = 32 // AES-256

// keyring wraps the master KEK. In prod the KEK is a KMS-resident key; here it is
// a 32-byte value from PROFILE_KEK (base64) or a per-process random key for
// dev/test. The KEK never encrypts PII directly — only DEKs.
type keyring struct {
	kek cipher.AEAD
}

func newKeyring(kek []byte) (*keyring, error) {
	if len(kek) != dekLen {
		return nil, fmt.Errorf("KEK must be %d bytes, got %d", dekLen, len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &keyring{kek: aead}, nil
}

// randomKey returns n cryptographically-random bytes.
func randomKey(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("identity-profile: entropy source failed: " + err.Error())
	}
	return b
}

// newDEK mints a fresh per-user data-encryption key.
func newDEK() []byte { return randomKey(dekLen) }

// wrapDEK seals a DEK under the KEK for storage in the keystore.
func (k *keyring) wrapDEK(dek []byte) (string, error) { return seal(k.kek, dek, nil) }

// unwrapDEK recovers a DEK from its wrapped form. A destroyed (empty) wrapped
// value yields errKeyDestroyed — the crypto-shred signal.
func (k *keyring) unwrapDEK(wrapped string) ([]byte, error) {
	if wrapped == "" {
		return nil, errKeyDestroyed
	}
	return open(k.kek, wrapped, nil)
}

// sealField encrypts one PII field under a DEK, binding it to the user token
// (AAD) so ciphertext is not portable across rows.
func sealField(dek []byte, plaintext, userToken string) (string, error) {
	aead, err := aeadFor(dek)
	if err != nil {
		return "", err
	}
	return seal(aead, []byte(plaintext), []byte(userToken))
}

// openField decrypts one PII field. With a destroyed DEK the caller never gets
// here (unwrapDEK already failed); a wrong DEK/AAD yields an auth error.
func openField(dek []byte, ciphertext, userToken string) (string, error) {
	aead, err := aeadFor(dek)
	if err != nil {
		return "", err
	}
	pt, err := open(aead, ciphertext, []byte(userToken))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func aeadFor(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal returns base64(nonce || AES-GCM ciphertext).
func seal(aead cipher.AEAD, plaintext, aad []byte) (string, error) {
	nonce := randomKey(aead.NonceSize())
	ct := aead.Seal(nil, nonce, plaintext, aad)
	out := append(nonce, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func open(aead cipher.AEAD, encoded string, aad []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, raw[:ns], raw[ns:], aad)
}
