package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// etag.go — optimistic-concurrency primitives (02 §1: "ETag/If-Match on mutable
// resources (menus, carts) → 412 on stale write"). The ETag is a STRONG,
// opaque validator derived from the resource id + its monotonic version. Every
// accepted mutation bumps the version, so the ETag a reader holds becomes stale
// the instant anyone else's write commits — which is exactly what makes a
// concurrent stale write detectable and rejectable with 412.

// makeETag derives the strong ETag for a resource at a given version. It is a
// short SHA-256 over "kind:id:version" wrapped in the required double quotes
// (RFC 7232). Opaque to clients: they must echo it verbatim in If-Match, never
// parse it.
func makeETag(kind, id string, version int64) string {
	h := sha256.Sum256([]byte(kind + ":" + id + ":" + itoa(version)))
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

// etagMatches reports whether a client-supplied If-Match header authorises a
// write against the resource's CURRENT etag. Per RFC 7232 a bare "*" matches any
// existing resource. Comma-separated lists are honoured (any member matching
// wins). Surrounding whitespace and weak-validator prefixes (W/) are tolerated.
func etagMatches(ifMatch, current string) bool {
	ifMatch = strings.TrimSpace(ifMatch)
	if ifMatch == "" {
		return false
	}
	if ifMatch == "*" {
		return true
	}
	for _, tok := range strings.Split(ifMatch, ",") {
		if normalizeETag(tok) == normalizeETag(current) {
			return true
		}
	}
	return false
}

// normalizeETag strips whitespace and an optional weak-validator W/ prefix so a
// client that sends W/"x" still matches a strong "x" for our purposes (we only
// mint strong tags; this is purely lenient parsing).
func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return s
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
