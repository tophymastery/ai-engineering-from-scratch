package factories

import (
	"fmt"

	"github.com/shop-platform/shop/libs/sharding"
)

// newID mints a platform shard-hint ULID (02 §1 / D6) DETERMINISTICALLY.
//
// The production sharding.NewID uses crypto/rand + wall time, so it can't be
// used for reproducible seeded data. Instead we build the exact same wire format
//
//	<prefix>_<HH><26-char Crockford ULID body>
//
// where HH is the real logical shard (sharding.LogicalShard of a deterministic
// per-entity key) and the 26-char body is Crockford-encoded from the factory's
// monotonic millisecond counter + 80 bits drawn from the injected seeded RNG.
// The result round-trips through sharding.Decode and passes sharding.ValidateBody
// (asserted in the tests), so factory IDs are indistinguishable from production
// IDs to any router — yet identical for a given seed.
func (f *Factory) newID(prefix string) string {
	f.n++
	key := fmt.Sprintf("%s:%d", prefix, f.n) // deterministic shard key
	shard := sharding.LogicalShard(key)      // real routing hint
	return fmt.Sprintf("%s_%02x%s", prefix, shard, f.ulidBody())
}

// ulidBody builds a 26-char Crockford ULID body: 48-bit monotonic ms prefix +
// 80 bits from the seeded RNG. f.ms advances one millisecond per ID so bodies
// stay time-sortable and unique within a factory, and fully deterministic.
func (f *Factory) ulidBody() string {
	var b [16]byte
	ms := f.ms
	f.ms++
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	for i := 6; i < 16; i++ {
		b[i] = byte(f.rnd.Intn(256))
	}
	return encodeCrockford(b)
}

// crockford is the Crockford base32 alphabet (excludes I, L, O, U) — the ULID
// alphabet, matching libs/sharding so bodies validate there.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodeCrockford renders 128 bits as the standard 26-char ULID base32 body.
// Bit-packing identical to libs/sharding.encodeCrockford (the wire contract).
func encodeCrockford(b [16]byte) string {
	var out [26]byte
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
