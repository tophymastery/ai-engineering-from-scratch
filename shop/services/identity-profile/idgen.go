package main

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// crock is Crockford base32 (no padding), the ULID alphabet (02 §1: prefixed
// ULIDs — self-describing, sortable, unguessable). Same codec as identity-auth.
var crock = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// newToken mints a prefixed ULID: <prefix>_<26-char Crockford base32 of a 48-bit
// ms timestamp + 80 random bits>. These are the ONLY user/address identifiers
// that ever leave identity-profile — events, order snapshots and logs carry
// `usr_`/`adr_` tokens, never PII (D3).
func newToken(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	_, _ = rand.Read(b[6:])
	return prefix + "_" + strings.ToLower(crock.EncodeToString(b[:]))
}

// tokenKind classifies a token by its prefix (used by the resolve endpoint and
// the order-replay path, both of which operate on tokens alone).
func tokenKind(tok string) string {
	switch {
	case strings.HasPrefix(tok, "usr_"):
		return "user"
	case strings.HasPrefix(tok, "adr_"):
		return "address"
	default:
		return ""
	}
}
