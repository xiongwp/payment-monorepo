module github.com/xiongwp/card-center

go 1.25.0

require (
	github.com/prometheus/client_golang v1.23.2
	github.com/spf13/viper v1.19.0
	github.com/xiongwp/kms-manage v0.0.0-00010101000000-000000000000
	github.com/xiongwp/payment-util v0.0.0-20260430112954-918ccf6739bd
	github.com/xiongwp/user-merchant-core v0.0.0-00010101000000-000000000000
	go.uber.org/fx v1.20.1
	go.uber.org/zap v1.27.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
)

// 联栈开发模式：直接 replace 到 monorepo 平级 sibling，避免 go get 私仓
replace (
	github.com/xiongwp/kms-manage         => ../kms-manage
	github.com/xiongwp/payment-util       => ../payment-util
	github.com/xiongwp/user-merchant-core => ../user-merchant-core
)
