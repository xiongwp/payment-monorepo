module github.com/xiongwp/card-payment

go 1.25.0

require (
	github.com/cloudwego/kitex v0.16.2
	github.com/prometheus/client_golang v1.23.2
	github.com/spf13/viper v1.19.0
	github.com/xiongwp/card-center v0.0.0-20260505000000-000000000000
	github.com/xiongwp/payment-util v0.0.1
	go.uber.org/fx v1.20.1
	go.uber.org/zap v1.27.1
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
	gorm.io/driver/mysql v1.5.7
	gorm.io/gorm v1.25.12
)

require (
	github.com/bytedance/gopkg v0.1.4 // indirect
	github.com/bytedance/sonic v1.15.1 // indirect
	github.com/bytedance/sonic/loader v0.5.1 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/cloudwego/dynamicgo v0.9.1 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	golang.org/x/arch v0.14.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
)

// 联栈开发模式：直接 replace 到 monorepo 平级 sibling
replace (
	github.com/xiongwp/card-center => ../card-center
	github.com/xiongwp/payment-util => ../payment-util
)
