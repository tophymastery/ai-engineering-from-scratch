package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing. The task allows a std-lib/x/crypto password KDF; we use
// PBKDF2-HMAC-SHA256 from the Go 1.24 std lib (crypto/pbkdf2) with a per-user
// 16-byte salt and a high iteration count — no cgo, no external module, and a
// self-describing encoded form so the cost can be raised later without a schema
// change. (bcrypt/argon2 would be equivalent; PBKDF2 keeps the build pure-stdlib.)
const (
	pbkdf2Iter = 210_000 // OWASP-scale work factor for PBKDF2-HMAC-SHA256
	pbkdf2Klen = 32
	saltLen    = 16
)

// hashPassword returns an encoded hash: pbkdf2-sha256$<iter>$<salt_b64>$<dk_b64>.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, pbkdf2Klen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// verifyPassword checks pw against an encoded hash in constant time.
func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// hashToken returns a SHA-256 hex of an opaque token — refresh tokens are stored
// only as this hash (a DB leak never yields a usable token).
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
