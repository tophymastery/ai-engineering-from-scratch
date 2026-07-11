package sharding

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Shard-hint prefixed ULID format (D6, 02 §1 IDs).
//
//	<prefix>_<HH><BODY>
//	  prefix : 02 §1 entity prefix, e.g. "ord", "usr", "pay" (no underscore)
//	  _      : single literal separator
//	  HH     : exactly 2 LOWERCASE hex chars = the logical shard 00..ff
//	  BODY   : a standard 26-char Crockford base32 ULID (48-bit ms time +
//	           80-bit randomness, monotonic within a millisecond)
//
// Example: ord_a301J9Z8P4Q2R7V6X0Y5M3K1BC — shard 0xa3 = 163.
//
// The hint sits BETWEEN the prefix and an otherwise-untouched 26-char ULID, so
// the body keeps full ULID randomness, lexicographic time ordering, and
// monotonicity; only the routing hint is added. A point lookup reads HH and
// goes straight to the physical shard with zero directory calls, and Decode
// recovers the same logical shard LogicalShard(entity_key) produced at creation.

const (
	// crockford is the Crockford base32 alphabet (excludes I, L, O, U).
	crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// ulidBodyLen is the fixed length of the 128-bit Crockford base32 body.
	ulidBodyLen = 26
	// hexShard is the width of the shard hint in hex chars.
	hexShardLen = 2
)

var (
	// ErrBadID is returned when an ID does not match the shard-hint format.
	ErrBadID = errors.New("sharding: malformed shard-hint ULID")

	crockfordRev [256]int8
	revOnce      sync.Once
)

func initRev() {
	for i := range crockfordRev {
		crockfordRev[i] = -1
	}
	for i, c := range crockford {
		crockfordRev[c] = int8(i)
		// accept lowercase too (Crockford is case-insensitive on decode).
		crockfordRev[c+('a'-'A')] = int8(i)
	}
}

// gen is the process-wide monotonic ULID body generator.
var gen = &monotonic{}

type monotonic struct {
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte // 80 bits of randomness
}

// NewID mints a shard-hint ULID for prefix whose hint encodes the logical shard
// of entityKey (the shard key: customer_id for orders, order_id for payments,
// etc.). Decode(NewID(p, k)) == LogicalShard(k) for every k — the property the
// 1M-ID agreement test asserts. Safe for concurrent use.
func NewID(prefix, entityKey string) string {
	return NewIDForShard(prefix, LogicalShard(entityKey))
}

// NewIDForShard mints a shard-hint ULID whose hint is exactly shard. Used when
// the caller already holds the logical shard (e.g. re-deriving an ID for a known
// row). Panics if shard is out of range — a programming error, not input error.
func NewIDForShard(prefix string, shard int) string {
	if shard < 0 || shard >= NumLogicalShards {
		panic(fmt.Sprintf("sharding: shard %d out of range [0,%d)", shard, NumLogicalShards))
	}
	var sb strings.Builder
	sb.Grow(len(prefix) + 1 + hexShardLen + ulidBodyLen)
	sb.WriteString(prefix)
	sb.WriteByte('_')
	const hexdigits = "0123456789abcdef"
	sb.WriteByte(hexdigits[(shard>>4)&0xf])
	sb.WriteByte(hexdigits[shard&0xf])
	sb.WriteString(gen.body())
	return sb.String()
}

// Decode recovers the logical shard from a shard-hint ULID. It reads only the 2
// hex chars after the '_' — O(1), no hashing, no directory. The prefix and body
// are validated for shape but their content is not required for routing.
func Decode(id string) (shard int, prefix string, err error) {
	us := strings.IndexByte(id, '_')
	if us < 0 || us == 0 {
		return 0, "", ErrBadID
	}
	prefix = id[:us]
	rest := id[us+1:]
	if len(rest) != hexShardLen+ulidBodyLen {
		return 0, "", ErrBadID
	}
	hi, ok1 := hexVal(rest[0])
	lo, ok2 := hexVal(rest[1])
	if !ok1 || !ok2 {
		return 0, "", ErrBadID
	}
	return hi<<4 | lo, prefix, nil
}

// DecodeShard is the routing-hot-path helper: shard only, ErrBadID on garbage.
func DecodeShard(id string) (int, error) {
	s, _, err := Decode(id)
	return s, err
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// body returns the next 26-char Crockford ULID body, monotonic within a ms.
func (m *monotonic) body() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ms := uint64(time.Now().UnixMilli())
	if ms > m.lastMS {
		m.lastMS = ms
		if _, err := rand.Read(m.lastRand[:]); err != nil {
			// crypto/rand failing is fatal for ID uniqueness; fall back to a
			// time-derived seed so we never emit a colliding all-zero body.
			seedFromTime(&m.lastRand, ms)
		}
	} else {
		// same or clock-regressed ms: keep the timestamp monotonic and bump the
		// 80-bit randomness by 1 (big-endian) to preserve strict ordering.
		ms = m.lastMS
		incr(&m.lastRand)
	}

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], m.lastRand[:])
	return encodeCrockford(b)
}

func incr(r *[10]byte) {
	for i := len(r) - 1; i >= 0; i-- {
		r[i]++
		if r[i] != 0 {
			return
		}
	}
	// overflow (astronomically unlikely within one ms): wrap, still unique
	// enough for a sandbox/reference codec.
}

func seedFromTime(r *[10]byte, ms uint64) {
	x := ms*0x9e3779b97f4a7c15 + 0x243f6a8885a308d3
	for i := range r {
		x = fmix64(x)
		r[i] = byte(x)
	}
}

// encodeCrockford renders 128 bits as 26 Crockford base32 chars (MSB first).
// 26*5 = 130 bits; the top 2 bits are zero-padded, matching the ULID spec.
func encodeCrockford(b [16]byte) string {
	var out [26]byte
	// Standard ULID base32 bit-packing (10-byte time+rand layout unrolled).
	out[0] = crockford[(b[0]&224)>>5]
	out[1] = crockford[b[0]&31]
	out[2] = crockford[(b[1]&248)>>3]
	out[3] = crockford[(b[1]&7)<<2|(b[2]&192)>>6]
	out[4] = crockford[(b[2]&62)>>1]
	out[5] = crockford[(b[2]&1)<<4|(b[3]&240)>>4]
	out[6] = crockford[(b[3]&15)<<1|(b[4]&128)>>7]
	out[7] = crockford[(b[4]&124)>>2]
	out[8] = crockford[(b[4]&3)<<3|(b[5]&224)>>5]
	out[9] = crockford[b[5]&31]
	out[10] = crockford[(b[6]&248)>>3]
	out[11] = crockford[(b[6]&7)<<2|(b[7]&192)>>6]
	out[12] = crockford[(b[7]&62)>>1]
	out[13] = crockford[(b[7]&1)<<4|(b[8]&240)>>4]
	out[14] = crockford[(b[8]&15)<<1|(b[9]&128)>>7]
	out[15] = crockford[(b[9]&124)>>2]
	out[16] = crockford[(b[9]&3)<<3|(b[10]&224)>>5]
	out[17] = crockford[b[10]&31]
	out[18] = crockford[(b[11]&248)>>3]
	out[19] = crockford[(b[11]&7)<<2|(b[12]&192)>>6]
	out[20] = crockford[(b[12]&62)>>1]
	out[21] = crockford[(b[12]&1)<<4|(b[13]&240)>>4]
	out[22] = crockford[(b[13]&15)<<1|(b[14]&128)>>7]
	out[23] = crockford[(b[14]&124)>>2]
	out[24] = crockford[(b[14]&3)<<3|(b[15]&224)>>5]
	out[25] = crockford[b[15]&31]
	return string(out[:])
}

// ValidateBody reports whether s is a syntactically valid 26-char Crockford
// ULID body. Used by tests to prove the emitted body stays spec-compliant.
func ValidateBody(s string) bool {
	revOnce.Do(initRev)
	if len(s) != ulidBodyLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if crockfordRev[s[i]] < 0 {
			return false
		}
	}
	return true
}
