module github.com/xiongwp/payment-admin-web/backend

go 1.25.0

require (
	github.com/cloudwego/kitex v0.16.2
	github.com/gorilla/mux v1.8.1
	github.com/prometheus/client_golang v1.23.2
	github.com/xiongwp/kms-manage v0.0.1
	github.com/xiongwp/order-core v0.0.1
	github.com/xiongwp/payment-core v0.0.1
	github.com/xiongwp/payment-util v0.0.1
	github.com/xiongwp/risk-manage v0.0.1
	github.com/xiongwp/split-payment v0.0.1
	github.com/xiongwp/user-merchant-core v0.0.1
	golang.org/x/time v0.5.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bytedance/gopkg v0.1.4 // indirect
	github.com/bytedance/sonic v1.15.1 // indirect
	github.com/bytedance/sonic/loader v0.5.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/cloudwego/configmanager v0.2.3 // indirect
	github.com/cloudwego/dynamicgo v0.9.1 // indirect
	github.com/cloudwego/fastpb v0.0.6 // indirect
	github.com/cloudwego/frugal v0.3.1 // indirect
	github.com/cloudwego/gopkg v0.2.0 // indirect
	github.com/cloudwego/localsession v0.2.1 // indirect
	github.com/cloudwego/netpoll v0.7.2 // indirect
	github.com/cloudwego/prutal v0.1.3 // indirect
	github.com/cloudwego/runtimex v0.1.1 // indirect
	github.com/cloudwego/thriftgo v0.4.5 // indirect
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/fatih/structtag v1.2.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/pprof v0.0.0-20240727154555-813a5fbdbec8 // indirect
	github.com/iancoleman/strcase v0.2.0 // indirect
	github.com/jhump/protoreflect v1.8.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.2.10 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tidwall/gjson v1.17.3 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	go.etcd.io/etcd/api/v3 v3.5.21 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.21 // indirect
	go.etcd.io/etcd/client/v3 v3.5.21 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.43.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/arch v0.14.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto v0.0.0-20240213162025-012b6fc9bca9 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Go 的 MVS 只认本 module 的 replace，不会继承 kms-manage / order-core 等兄弟仓
// 自己的 replace，所以这里要把每一个【被 require 的兄弟仓，以及它们的 replace
// 链再传递一层】都独立列一遍。
// accounting-grpc-api 来自 order-core 的 replace 链（order-core 的 go.mod
// 又 replace 了 accounting-grpc-api）—— MVS 不继承，这里必须显式再 replace。
replace (
	github.com/xiongwp/accounting-grpc-api => ../../accounting-grpc-api
	github.com/xiongwp/kms-manage => ../../kms-manage
	github.com/xiongwp/order-core => ../../order-core
	github.com/xiongwp/payment-channel => ../../payment-channel
	github.com/xiongwp/payment-core => ../../payment-core
	github.com/xiongwp/risk-manage => ../../risk-manage
	github.com/xiongwp/split-payment => ../../split-payment
	github.com/xiongwp/user-merchant-core => ../../user-merchant-core
)

replace github.com/xiongwp/payment-util => ../../payment-util
