package sharding

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDecodeAgrees1M is the D6 test-criterion: "shard-hint decode agrees with
// hash routing on 100% of 1M generated IDs." For 1,000,000 distinct entity keys
// we mint a shard-hint ULID and assert the decoded hint equals the independently
// computed LogicalShard(key) — proving a point lookup can route from the ID
// alone with zero directory reads. Also asserts every ID's body is a valid
// 26-char Crockford ULID and that all 256 shards are exercised. In-memory, <60 s.
func TestDecodeAgrees1M(t *testing.T) {
	const N = 1_000_000
	start := time.Now()
	var seen [NumLogicalShards]bool
	mismatches := 0
	badBody := 0
	for i := 0; i < N; i++ {
		key := "cus_" + strconv.Itoa(i)
		want := LogicalShard(key)
		id := NewID("ord", key)
		got, prefix, err := Decode(id)
		if err != nil || prefix != "ord" {
			mismatches++
			continue
		}
		if got != want {
			mismatches++
			continue
		}
		seen[want] = true
		// body = everything after "<prefix>_<2 hex>"
		body := id[len("ord")+1+hexShardLen:]
		if !ValidateBody(body) {
			badBody++
		}
	}
	dur := time.Since(start)

	covered := 0
	for _, s := range seen {
		if s {
			covered++
		}
	}
	agreementPct := 100 * float64(N-mismatches) / float64(N)
	t.Logf("N=%d decode-agreement=%.4f%% mismatches=%d bad_bodies=%d shards_covered=%d/256 took=%s",
		N, agreementPct, mismatches, badBody, covered, dur)

	if mismatches != 0 {
		t.Fatalf("decode disagreed with hash routing on %d/%d IDs (want 0)", mismatches, N)
	}
	if badBody != 0 {
		t.Fatalf("%d/%d IDs had a non-Crockford ULID body", badBody, N)
	}
	if covered != NumLogicalShards {
		t.Fatalf("shard hint covered only %d/256 shards", covered)
	}
	if dur > 60*time.Second {
		t.Fatalf("1M decode test took %s (> 60s budget)", dur)
	}
}

func TestULIDFormat(t *testing.T) {
	id := NewIDForShard("ord", 163) // 0xa3
	if !strings.HasPrefix(id, "ord_a3") {
		t.Fatalf("expected prefix ord_a3, got %q", id)
	}
	if len(id) != len("ord_")+hexShardLen+ulidBodyLen {
		t.Fatalf("unexpected id length %d for %q", len(id), id)
	}
	shard, prefix, err := Decode(id)
	if err != nil || prefix != "ord" || shard != 163 {
		t.Fatalf("decode(%q) = (%d,%q,%v), want (163,ord,nil)", id, shard, prefix, err)
	}
	if !ValidateBody(id[len("ord_a3"):]) {
		t.Fatalf("body of %q is not a valid Crockford ULID", id)
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []string{
		"",                              // empty
		"nounderscore",                  // no separator
		"_a301J9Z8P4Q2R7V6X0Y5M3K1BC",   // empty prefix
		"ord_zz01J9Z8P4Q2R7V6X0Y5M3K1B", // non-hex hint
		"ord_a3",                        // no body
		"ord_a301J9Z8P4Q2R7V6X0Y5M3K1", // body too short (25)
	}
	for _, c := range cases {
		if _, _, err := Decode(c); err == nil {
			t.Errorf("Decode(%q) = nil error, want ErrBadID", c)
		}
	}
}

// TestULIDMonotonic asserts bodies minted within the same millisecond are
// strictly increasing (ULID monotonicity requirement).
func TestULIDMonotonic(t *testing.T) {
	prev := ""
	for i := 0; i < 100_000; i++ {
		id := NewIDForShard("ord", 10)
		body := id[len("ord_a"):] // arbitrary slice; compare full ids of same shard
		_ = body
		full := id
		if prev != "" && full <= prev {
			t.Fatalf("non-monotonic at i=%d: %q <= %q", i, full, prev)
		}
		prev = full
	}
}

func TestNewIDForShardRange(t *testing.T) {
	for s := 0; s < NumLogicalShards; s++ {
		id := NewIDForShard("usr", s)
		got, _, err := Decode(id)
		if err != nil || got != s {
			t.Fatalf("shard %d round-trip failed: got %d err %v", s, got, err)
		}
	}
}
