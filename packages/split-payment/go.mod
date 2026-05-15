module reconcile-system/packages/split-payment

go 1.22

require (
	github.com/go-sql-driver/mysql v1.8.1            // MF-1: MySQL driver
	github.com/twmb/franz-go v1.18.0                 // SP-8/11: Kafka client (events + refund subscriber)
	github.com/xiongwp/accounting-system v0.0.0
	go.uber.org/zap v1.27.0
	google.golang.org/grpc v1.62.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.9.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/net v0.21.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240227224415-6ceb2ff114de // indirect
	google.golang.org/protobuf v1.32.0 // indirect
)

// 监本仓库
replace github.com/xiongwp/accounting-system => ../accounting-system
