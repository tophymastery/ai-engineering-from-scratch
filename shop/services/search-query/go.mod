module github.com/shop-platform/shop/services/search-query

go 1.24

require (
	github.com/shop-platform/shop/libs/errors v0.0.0
	github.com/shop-platform/shop/libs/eventbus v0.0.0
	github.com/shop-platform/shop/libs/flags v0.0.0
	github.com/shop-platform/shop/libs/logging v0.0.0
	github.com/shop-platform/shop/libs/otel v0.0.0
	github.com/shop-platform/shop/libs/testhooks v0.0.0
	github.com/shop-platform/shop/services/search-indexer v0.0.0
)

require (
	github.com/shop-platform/shop/libs/inbox v0.0.0 // indirect
	github.com/shop-platform/shop/libs/sharding v0.0.0 // indirect
)

replace github.com/shop-platform/shop/libs/errors => ../../libs/errors

replace github.com/shop-platform/shop/libs/eventbus => ../../libs/eventbus

replace github.com/shop-platform/shop/libs/flags => ../../libs/flags

replace github.com/shop-platform/shop/libs/inbox => ../../libs/inbox

replace github.com/shop-platform/shop/libs/logging => ../../libs/logging

replace github.com/shop-platform/shop/libs/otel => ../../libs/otel

replace github.com/shop-platform/shop/libs/sharding => ../../libs/sharding

replace github.com/shop-platform/shop/libs/testhooks => ../../libs/testhooks

replace github.com/shop-platform/shop/services/search-indexer => ../search-indexer
