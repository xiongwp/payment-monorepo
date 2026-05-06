module github.com/xiongwp/config-center

go 1.25.0

require (
	github.com/prometheus/client_golang v1.23.2
	github.com/spf13/viper v1.19.0
	github.com/xiongwp/payment-util v0.0.0-20260430112954-918ccf6739bd
	go.etcd.io/etcd/client/v3 v3.5.13
	go.uber.org/fx v1.20.1
	go.uber.org/zap v1.27.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
	gorm.io/driver/mysql v1.5.7
	gorm.io/gorm v1.25.12
)

replace github.com/xiongwp/payment-util => ../payment-util
