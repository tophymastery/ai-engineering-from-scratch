package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"strings"
	"time"
)

// crock is Crockford base32 (no padding), the ULID alphabet (02 §1: prefixed
// ULIDs — self-describing, sortable, unguessable).
var crock = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// newID mints a prefixed ULID: <prefix>_<26-char Crockford base32 of 48-bit ms
// timestamp + 80 random bits>. Sortable by creation time, unguessable.
func newID(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	// 48-bit big-endian timestamp in the first 6 bytes.
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	_, _ = rand.Read(b[6:])
	return prefix + "_" + strings.ToLower(crock.EncodeToString(b[:]))
}

// newOpaqueToken mints a high-entropy opaque refresh token (never a JWT). 256
// random bits, base32 Crockford — server-side stored only as a hash.
func newOpaqueToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "rft_" + strings.ToLower(crock.EncodeToString(b[:]))
}

// newJTI mints a random access-token id (the denylist key).
func newJTI() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "jti_" + strings.ToLower(crock.EncodeToString(b[:]))
}

var _ = binary.BigEndian // reserved for future monotonic sequencing
