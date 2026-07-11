module github.com/shop-platform/shop/tools/seedctl

go 1.24

require (
	github.com/shop-platform/shop/libs/factories v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/shop-platform/shop/libs/sharding v0.0.0 // indirect

replace github.com/shop-platform/shop/libs/factories => ../../libs/factories

replace github.com/shop-platform/shop/libs/sharding => ../../libs/sharding
