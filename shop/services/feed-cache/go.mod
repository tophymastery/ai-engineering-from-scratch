module github.com/shop-platform/shop/services/feed-cache

go 1.24

replace github.com/shop-platform/shop/libs/errors => ../../libs/errors

replace github.com/shop-platform/shop/libs/flags => ../../libs/flags

replace github.com/shop-platform/shop/libs/logging => ../../libs/logging

replace github.com/shop-platform/shop/libs/otel => ../../libs/otel

replace github.com/shop-platform/shop/libs/testhooks => ../../libs/testhooks

require (
	github.com/shop-platform/shop/libs/errors v0.0.0
	github.com/shop-platform/shop/libs/flags v0.0.0-00010101000000-000000000000
	github.com/shop-platform/shop/libs/logging v0.0.0-00010101000000-000000000000
	github.com/shop-platform/shop/libs/otel v0.0.0
	github.com/shop-platform/shop/libs/testhooks v0.0.0
)
