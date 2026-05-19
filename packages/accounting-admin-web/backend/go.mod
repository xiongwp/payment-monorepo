module github.com/xiongwp/accounting-admin-web/backend

go 1.25.0

require (
	github.com/gorilla/mux v1.8.1
	github.com/xiongwp/accounting-grpc-api v0.1.0
	// accounting-system 源仓 kitex_gen 直引用 (handler / cmd 大量 import).
	// 通过 docker-compose additional_contexts + replace 接进来.
	github.com/xiongwp/accounting-system v0.0.1
	// 给 accounting-system gRPC 拨号用的 etcd resolver 包。
	// 联栈多 pod 部署后 "accounting-service" 跨 compose 项目无法 DNS 解析，
	// 必须走 etcd 拿副本列表。
	github.com/xiongwp/payment-util v0.0.1
	google.golang.org/grpc v1.80.0
)

require (
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	go.etcd.io/etcd/api/v3 v3.5.21 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.21 // indirect
	go.etcd.io/etcd/client/v3 v3.5.21 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/xiongwp/accounting-grpc-api => ../../accounting-grpc-api

// payment-util 在仓 root 同级；backend 在 accounting-admin-web/backend/ 下，
// 所以相对路径要走两级 ../../。
replace github.com/xiongwp/payment-util => ../../payment-util

// accounting-system 同模式: docker build 走 additional_contexts; 本地 dev 走 sibling.
replace github.com/xiongwp/accounting-system => ../../accounting-system
