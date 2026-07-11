module github.com/shop-platform/shop/services/placeholder

go 1.24

require (
	github.com/shop-platform/shop/libs/errors v0.0.0
	github.com/shop-platform/shop/libs/flags v0.0.0
	github.com/shop-platform/shop/libs/idempotency v0.0.0
	github.com/shop-platform/shop/libs/logging v0.0.0
	github.com/shop-platform/shop/libs/otel v0.0.0
	github.com/shop-platform/shop/libs/testhooks v0.0.0
)

replace github.com/shop-platform/shop/libs/testhooks => ../../libs/testhooks

replace github.com/shop-platform/shop/libs/errors => ../../libs/errors

replace github.com/shop-platform/shop/libs/flags => ../../libs/flags

replace github.com/shop-platform/shop/libs/idempotency => ../../libs/idempotency

replace github.com/shop-platform/shop/libs/logging => ../../libs/logging

replace github.com/shop-platform/shop/libs/otel => ../../libs/otel
