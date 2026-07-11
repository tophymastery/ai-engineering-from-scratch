module github.com/shop-platform/shop/services/identity-auth

go 1.24

require (
	github.com/shop-platform/shop/libs/edgeauth v0.0.0
	github.com/shop-platform/shop/libs/errors v0.0.0
	github.com/shop-platform/shop/libs/flags v0.0.0
	github.com/shop-platform/shop/libs/logging v0.0.0
	github.com/shop-platform/shop/libs/otel v0.0.0
	github.com/shop-platform/shop/libs/testhooks v0.0.0
	modernc.org/sqlite v1.34.5
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.22.0 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
)

replace github.com/shop-platform/shop/libs/edgeauth => ../../libs/edgeauth

replace github.com/shop-platform/shop/libs/errors => ../../libs/errors

replace github.com/shop-platform/shop/libs/flags => ../../libs/flags

replace github.com/shop-platform/shop/libs/logging => ../../libs/logging

replace github.com/shop-platform/shop/libs/otel => ../../libs/otel

replace github.com/shop-platform/shop/libs/testhooks => ../../libs/testhooks
