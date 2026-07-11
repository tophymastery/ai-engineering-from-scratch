module github.com/shop-platform/shop/libs/logging

go 1.24

require (
	github.com/shop-platform/shop/libs/errors v0.0.0
	github.com/shop-platform/shop/libs/otel v0.0.0
)

replace github.com/shop-platform/shop/libs/errors => ../errors

replace github.com/shop-platform/shop/libs/otel => ../otel
