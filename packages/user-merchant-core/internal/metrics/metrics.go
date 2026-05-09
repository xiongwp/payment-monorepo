// Package metrics exposes Prometheus counters / histograms for user-merchant-core.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// ─── Merchant ────────────────────────────────────────────────────────────────

// MerchantCreatedTotal 商户注册计数
var MerchantCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_created_total",
	Help: "Total merchant registrations",
}, []string{"country"})

// MerchantKYCTransitionTotal KYC 状态流转计数
var MerchantKYCTransitionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_kyc_transition_total",
	Help: "Merchant KYC status transitions",
}, []string{"from", "to"})

// ─── Merchant Secret ────────────────────────────────────────────────────────

// MerchantSecretOpTotal 凭据存取操作计数
var MerchantSecretOpTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_secret_op_total",
	Help: "Merchant channel secret ops by op + result",
}, []string{"op", "result"})

// MerchantCacheLookupTotal 缓存查询结果计数（hit/miss），按索引维度细分
// （byID / byKeyHash）。没有它根本没法判断 cache.size / cache.ttl 是否合理。
var MerchantCacheLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_cache_lookup_total",
	Help: "Merchant cache lookups by index + result (hit/miss)",
}, []string{"index", "result"})

// MerchantCacheSize 当前缓存条目数（两个索引各一条 series）。
var MerchantCacheSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "user_merchant_cache_size",
	Help: "Current number of entries in each merchant cache index",
}, []string{"index"})

// MerchantSecretPlaintextCacheLookupTotal BulkGetPlaintext 解密结果缓存命中情况；
// 每次 miss 都等于一次 KMS Decrypt（昂贵）。
var MerchantSecretPlaintextCacheLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_secret_plaintext_cache_lookup_total",
	Help: "Merchant secret plaintext cache lookups by result (hit/miss)",
}, []string{"result"})

// IntrospectCacheLookupTotal IntrospectToken 进程内缓存命中情况。
// 期望命中率 90%+ 才算真省 DB；< 70% 表示 TTL 太短或 cap 太小。
//
// 关联告警：cache hit rate < 70% 持续 15min → P2（容量告警），
// 见 deploy/alertmanager/payment-rules.yml。
var IntrospectCacheLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_introspect_cache_lookup_total",
	Help: "IntrospectToken cache lookups by result (hit/miss)",
}, []string{"result"})

// KMSRPCTotal / KMSRPCDuration 追踪每次调 kms-manage 的成功率和耗时；
// 线上 kms-manage 抖动会直接影响 Put/BulkGetPlaintext 的 p99，必须可观测。
var KMSRPCTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_kms_rpc_total",
	Help: "Calls to kms-manage gRPC by op + result",
}, []string{"op", "result"})

var KMSRPCDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "user_merchant_kms_rpc_duration_seconds",
	Help:    "kms-manage RPC latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"op"})

// ─── gRPC ────────────────────────────────────────────────────────────────────

// GRPCRequestTotal gRPC 请求计数
var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "user_merchant_grpc_request_total",
	Help: "gRPC requests by method + code",
}, []string{"method", "code"})

// GRPCRequestDuration gRPC 请求耗时
var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "user_merchant_grpc_request_duration_seconds",
	Help:    "gRPC request latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"method"})

// Register 把所有指标注册到默认 registry（在 main 里调用一次）
func Register() {
	prometheus.MustRegister(
		MerchantCreatedTotal,
		MerchantKYCTransitionTotal,
		MerchantSecretOpTotal,
		MerchantCacheLookupTotal,
		MerchantCacheSize,
		MerchantSecretPlaintextCacheLookupTotal,
		IntrospectCacheLookupTotal,
		KMSRPCTotal,
		KMSRPCDuration,
		GRPCRequestTotal,
		GRPCRequestDuration,
	)
}

// HealthChecker 供 /healthz 聚合的探针。实现只需要一个 name + 一个快速探活函数。
// ping 超时（常用 500ms）由调用方自行用 ctx 控制，这里只负责调度并汇总。
type HealthChecker interface {
	Name() string
	Ping(context.Context) error
}

// StartServer 启动 /metrics + /healthz HTTP（在 goroutine 中，非阻塞）。
//
// /healthz 会对所有 checker 做 Ping；任一失败就返回 503，并在 body 里列出
// 谁挂了。没有 checker 时退化到之前的"ok"。超时固定 500ms，避免把 k8s liveness
// 拖慢。
func StartServer(addr string, logger *zap.Logger, checkers ...HealthChecker) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if len(checkers) == 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancel()
		var failures []string
		for _, c := range checkers {
			if err := c.Ping(ctx); err != nil {
				failures = append(failures, c.Name()+": "+err.Error())
			}
		}
		if len(failures) > 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			for _, f := range failures {
				_, _ = w.Write([]byte(f + "\n"))
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	go func() {
		logger.Info("metrics http listening", zap.String("addr", addr))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}
