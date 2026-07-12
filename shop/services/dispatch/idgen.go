package main

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// crock is Crockford base32 (no padding), the ULID alphabet (02 §1: prefixed
// ULIDs). Same codec as the other slices so all IDs share one format.
var crock = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// newToken mints a prefixed ULID. Prefixes used here: asg_ (assignment),
// drv_ (synthetic driver, S-T8 compat), evt_ (event).
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
