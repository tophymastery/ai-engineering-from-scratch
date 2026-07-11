package main

import (
	"strings"
	"testing"
)

func TestEnvelopeEncryptionRoundTrip(t *testing.T) {
	kr, err := newKeyring(randomKey(dekLen))
	if err != nil {
		t.Fatal(err)
	}
	dek := newDEK()
	wrapped, err := kr.wrapDEK(dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kr.unwrapDEK(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(dek) {
		t.Fatal("unwrapped DEK != original")
	}

	ct, err := sealField(dek, "Budi Santoso", "usr_abc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, "Budi") {
		t.Fatalf("ciphertext leaks plaintext: %q", ct)
	}
	pt, err := openField(dek, ct, "usr_abc")
	if err != nil {
		t.Fatal(err)
	}
	if pt != "Budi Santoso" {
		t.Fatalf("decrypt mismatch: %q", pt)
	}
}

// TestWrongAADRejected — a ciphertext sealed for one user can't be opened under
// another user token (AAD binding), so rows aren't portable across users.
func TestWrongAADRejected(t *testing.T) {
	dek := newDEK()
	ct, _ := sealField(dek, "+65-9123-4567", "usr_owner")
	if _, err := openField(dek, ct, "usr_attacker"); err == nil {
		t.Fatal("decrypt under wrong AAD (user token) should fail")
	}
}

// TestDestroyedKeyIsUnreadable — the crypto-shred primitive: once the wrapped DEK
// is destroyed (empty), unwrap yields errKeyDestroyed and no plaintext is
// recoverable, even though the KEK is intact.
func TestDestroyedKeyIsUnreadable(t *testing.T) {
	kr, _ := newKeyring(randomKey(dekLen))
	if _, err := kr.unwrapDEK(""); err != errKeyDestroyed {
		t.Fatalf("destroyed key should yield errKeyDestroyed, got %v", err)
	}
}
