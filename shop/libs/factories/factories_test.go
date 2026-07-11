package factories

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shop-platform/shop/libs/sharding"
)

// buildAll produces one of every core entity from a fresh factory, in a fixed
// order, so two runs with the same seed can be compared byte-for-byte.
func buildAll(seed int64) []any {
	f := New(seed, Region("bkk"))
	u := f.User()
	m := f.Merchant()
	i := f.MenuItem(WithMenuMerchant(m.ID))
	c := f.Cart(WithCartUser(u.ID), WithCartMerchant(m.ID), WithCartLines(CartLine{MenuItemID: i.ID, Qty: 2}))
	d := f.Driver(WithDriverOnline(true))
	o := f.Order(WithUser(u.ID), WithMerchant(m.ID), WithDriver(d.ID), WithStatus("DELIVERED"))
	return []any{u, m, i, c, d, o}
}

// TestSameSeedByteIdentical is the core repeatability contract (03 §4): same
// seed ⇒ byte-identical entities.
func TestSameSeedByteIdentical(t *testing.T) {
	a, _ := json.Marshal(buildAll(42))
	b, _ := json.Marshal(buildAll(42))
	if string(a) != string(b) {
		t.Fatalf("same seed produced different entities:\n a=%s\n b=%s", a, b)
	}
	c, _ := json.Marshal(buildAll(43))
	if string(a) == string(c) {
		t.Fatal("different seed produced identical entities — RNG not seed-driven")
	}
}

// TestEveryEntityHasFactory asserts one factory per core entity and that each ID
// carries the correct 02 §1 prefix.
func TestEveryEntityHasFactory(t *testing.T) {
	f := New(1)
	checks := []struct {
		prefix string
		id     string
	}{
		{"usr", f.User().ID},
		{"mer", f.Merchant().ID},
		{"itm", f.MenuItem().ID},
		{"crt", f.Cart().ID},
		{"drv", f.Driver().ID},
		{"ord", f.Order().ID},
	}
	for _, c := range checks {
		if !strings.HasPrefix(c.id, c.prefix+"_") {
			t.Fatalf("expected prefix %q, got id %q", c.prefix, c.id)
		}
	}
}

// TestIDsAreValidShardHintULIDs proves factory IDs are real platform IDs: they
// decode through libs/sharding and their body is a valid Crockford ULID.
func TestIDsAreValidShardHintULIDs(t *testing.T) {
	f := New(7)
	ids := []string{f.User().ID, f.Merchant().ID, f.MenuItem().ID, f.Cart().ID, f.Driver().ID, f.Order().ID}
	seen := map[string]bool{}
	for _, id := range ids {
		shard, prefix, err := sharding.Decode(id)
		if err != nil {
			t.Fatalf("sharding.Decode(%q): %v", id, err)
		}
		if shard < 0 || shard >= sharding.NumLogicalShards {
			t.Fatalf("id %q shard %d out of range", id, shard)
		}
		// body = everything after "<prefix>_HH"
		body := id[len(prefix)+1+2:]
		if !sharding.ValidateBody(body) {
			t.Fatalf("id %q body %q is not a valid Crockford ULID", id, body)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// TestOverridesApply confirms functional options override defaults (03 §3).
func TestOverridesApply(t *testing.T) {
	f := New(5)
	o := f.Order(WithStatus("DELIVERED"), WithRegion("cnx"), WithTotal(12550))
	if o.Status != "DELIVERED" || o.Region != "cnx" || o.Total.Amount != 12550 {
		t.Fatalf("overrides not applied: %+v", o)
	}
	if o.Total.Currency != "THB" {
		t.Fatalf("default currency lost: %+v", o.Total)
	}
}
