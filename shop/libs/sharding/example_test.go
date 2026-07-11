package sharding_test

import (
	"fmt"

	"github.com/shop-platform/shop/libs/sharding"
)

// ExampleCluster shows the full path: an entity key routes to a logical shard, a
// shard-hint ULID minted for that key decodes back to the same shard (no
// directory), and the sandbox cluster stores/reads it on the right physical
// target.
func ExampleCluster() {
	cfg, _ := sharding.LoadConfig("testdata/routing.4x256.json")
	cl, _ := sharding.NewClusterFromConfig(cfg)

	custID := "cus_01HCUSTOMEREXAMPLE"
	shard := sharding.LogicalShard(custID)

	// Mint an order ID whose 2-hex hint encodes the customer's shard.
	orderID := sharding.NewID("ord", custID)
	hintShard, prefix, _ := sharding.Decode(orderID)

	target, _ := cl.Put(custID, "order-payload")
	got, _ := cl.Get(custID)

	fmt.Printf("shard=%d hint=%d prefix=%s agree=%v stored_on=%s read=%s\n",
		shard, hintShard, prefix, shard == hintShard, target, got)
	// Output:
	// shard=39 hint=39 prefix=ord agree=true stored_on=pg-0 read=order-payload
}
