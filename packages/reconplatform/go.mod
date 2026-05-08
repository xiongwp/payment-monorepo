module reconcile-system

go 1.26

require (
	github.com/expr-lang/expr v1.17.8
	github.com/go-sql-driver/mysql v1.8.1
	github.com/prometheus/client_golang v1.19.1
	github.com/redis/go-redis/v9 v9.18.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/twmb/franz-go v1.20.7
	github.com/xiongwp/payment-util v0.0.0
	go.uber.org/zap v1.27.0
)

replace github.com/xiongwp/payment-util => ../payment-util

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/klauspost/compress v1.18.4 // indirect
	github.com/pierrec/lz4/v4 v4.1.25 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.12.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
)
