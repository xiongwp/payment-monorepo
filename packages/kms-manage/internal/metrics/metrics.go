// Package metrics exposes Prometheus counters / histograms for kms-manage.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xiongwp/payment-util/healthx"
	"go.uber.org/zap"
)

// KMSOpTotal 按操作 + 结果维度统计。op: encrypt/decrypt/gen_dek；result: ok/err/no_key/bad_format
var KMSOpTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kms_op_total",
	Help: "KMS operations by op + result",
}, []string{"op", "result"})

var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kms_grpc_request_total",
	Help: "gRPC requests by method + code",
}, []string{"method", "code"})

var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "kms_grpc_request_duration_seconds",
	Help:    "gRPC request latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"method"})

var once sync.Once

// Register 注册所有指标；幂等，多次调用没副作用（测试里需要）。
func Register() {
	once.Do(func() {
		prometheus.MustRegister(
			KMSOpTotal,
			GRPCRequestTotal,
			GRPCRequestDuration,
		)
	})
}

// KeystoreReadiness 注入到 healthx.Readiness 的接口：调用方提供"列出活跃
// 密钥"的能力。我们不在 metrics 包 import keystore（防循环依赖），而是接受
// 一个轻量函数。
type KeystoreReadiness func() (active string, count int, err error)

// StartServer 起一个 HTTP server 暴露 /metrics + /healthz + /readyz。
//
// keystore 为 nil 时退到老行为（/readyz 永远 OK，但 logger 打 WARN 提示
// 生产应配 readiness probe）。
func StartServer(addr string, logger *zap.Logger, ks KeystoreReadiness) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", healthx.Liveness)

	probes := []healthx.Probe{}
	if ks != nil {
		probes = append(probes, healthx.ProbeFunc{
			N: "keystore",
			F: func(_ context.Context) error {
				active, count, err := ks()
				if err != nil {
					return err
				}
				if active == "" {
					return fmt.Errorf("no active key")
				}
				if count == 0 {
					return fmt.Errorf("no keys loaded")
				}
				return nil
			},
		})
	} else {
		logger.Warn("readiness keystore probe not configured; /readyz will always pass")
	}
	mux.HandleFunc("/readyz", healthx.Readiness(probes...))

	go func() {
		logger.Info("metrics http listening", zap.String("addr", addr))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}
