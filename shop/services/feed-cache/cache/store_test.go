package cache

import (
	"testing"
	"time"
)

// TestTTLStore_FreshStaleMiss walks a value through the fresh → stale → miss
// windows on an advancing ManualClock (no wall-time sleeps).
func TestTTLStore_FreshStaleMiss(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	s := NewTTLStore(clk, 2*time.Second, 10*time.Second) // fresh 2s, hard 10s
	s.Set("k", []byte("v"))

	if v, st := s.Get("k"); st != Fresh || string(v) != "v" {
		t.Fatalf("t=0: got %s/%q, want fresh/v", st, v)
	}
	clk.Advance(1900 * time.Millisecond)
	if _, st := s.Get("k"); st != Fresh {
		t.Fatalf("t=1.9s: got %s, want fresh", st)
	}
	clk.Advance(200 * time.Millisecond) // t=2.1s → past fresh, within hard TTL
	if v, st := s.Get("k"); st != Stale || string(v) != "v" {
		t.Fatalf("t=2.1s: got %s/%q, want stale/v", st, v)
	}
	clk.Advance(8 * time.Second) // t=10.1s → past hard TTL
	if _, st := s.Get("k"); st != Miss {
		t.Fatalf("t=10.1s: got %s, want miss", st)
	}
}

// TestTTLStore_PlainTTL confirms freshTTL==ttl gives a hard fresh→miss with no
// stale band (the L1/L2 tier config for the merchant two-tier cache).
func TestTTLStore_PlainTTL(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	s := NewTTLStore(clk, time.Second, time.Second)
	s.Set("k", []byte("v"))
	if _, st := s.Get("k"); st != Fresh {
		t.Fatalf("want fresh")
	}
	clk.Advance(1100 * time.Millisecond)
	if _, st := s.Get("k"); st != Miss {
		t.Fatalf("want miss (no stale band), got %s", st)
	}
}

// TestTTLStore_DeleteAndCopy confirms invalidation and that stored values are
// copied (a caller mutating its buffer cannot corrupt the entry).
func TestTTLStore_DeleteAndCopy(t *testing.T) {
	s := NewTTLStore(SystemClock{}, time.Minute, time.Minute)
	buf := []byte("abc")
	s.Set("k", buf)
	buf[0] = 'X'
	if v, _ := s.Get("k"); string(v) != "abc" {
		t.Fatalf("stored value not copied: %q", v)
	}
	s.Delete("k")
	if _, st := s.Get("k"); st != Miss {
		t.Fatalf("after delete want miss, got %s", st)
	}
}
