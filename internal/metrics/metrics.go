// Package metrics exposes Prometheus counters / histograms for payment-channel.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xiongwp/payment-util/healthx"
	"go.uber.org/zap"
)

// AcquirerCallTotal 每个 adapter 动作的调用计数
var AcquirerCallTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paychan_acquirer_call_total",
	Help: "Acquirer calls by adapter + action + result",
}, []string{"adapter", "action", "result"})

// AcquirerCallDuration RPC 耗时
var AcquirerCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paychan_acquirer_call_duration_seconds",
	Help:    "Acquirer RPC call latency",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"adapter", "action"})

// WebhookReceivedTotal 入站 webhook 计数
var WebhookReceivedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paychan_webhook_received_total",
	Help: "Inbound webhooks by adapter + signature_ok + dedupe_hit",
}, []string{"adapter", "signature_ok", "dedupe"})

// WebhookForwardTotal 转发给 order-core 的结果
var WebhookForwardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paychan_webhook_forward_total",
	Help: "Webhook forwards to order-core by result",
}, []string{"adapter", "result"})

// IdempotentReplayTotal 幂等命中回放次数
var IdempotentReplayTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paychan_idempotent_replay_total",
	Help: "Idempotent replays by adapter + action",
}, []string{"adapter", "action"})

// IdempotentLookupTotal 幂等键查询的命中分布。结合 IdempotentReplayTotal 即可
// 算出 hit ratio。source=cache 表示进程内 LRU 缓存命中（DB 0 query）；
// source=db 表示落库查询命中；result=miss 表示首次请求（即将走 Insert）。
var IdempotentLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paychan_idempotent_lookup_total",
	Help: "Idempotency lookup outcomes by adapter + action + source + result",
}, []string{"adapter", "action", "source", "result"})

// GRPCRequestTotal gRPC 请求计数
var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paychan_grpc_request_total",
	Help: "gRPC requests by method + code",
}, []string{"method", "code"})

var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paychan_grpc_request_duration_seconds",
	Help:    "gRPC request latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"method"})

func Register() {
	prometheus.MustRegister(
		AcquirerCallTotal,
		AcquirerCallDuration,
		WebhookReceivedTotal,
		WebhookForwardTotal,
		IdempotentReplayTotal,
		IdempotentLookupTotal,
		GRPCRequestTotal,
		GRPCRequestDuration,
	)
}

// draining SIGTERM 收到后置 true，/readyz 立刻 503，k8s / LB 摘流量。
var draining atomic.Bool

// BeginDrain 标记本实例进入 drain。
func BeginDrain() { draining.Store(true) }

// ShardPinger 用于 readiness probe：调用方把 DB Manager.GetShard 闭起来传进来。
// 我们随便挑一个 shard ping，证明至少一个分片可达；不全 ping 防止级联（单库
// 卡住把 readiness 拖到超时）。
type ShardPinger func(ctx context.Context) error

// StartServer 起 HTTP server 暴露 /metrics + /healthz + /readyz。
//
// dbPing 为 nil 时退到老行为（仅 drain 检查 + 始终 200 if not draining），
// 让其它入口（mockserver / loadtest）继续编译。
func StartServer(addr string, logger *zap.Logger, dbPing ShardPinger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", healthx.Liveness)

	probes := []healthx.Probe{
		// drain probe 始终在；SIGTERM 后第一时间 fail
		healthx.ProbeFunc{
			N: "drain",
			F: func(_ context.Context) error {
				if draining.Load() {
					return fmt.Errorf("draining")
				}
				return nil
			},
		},
	}
	if dbPing != nil {
		probes = append(probes, healthx.ProbeFunc{N: "db_shard", F: dbPing})
	} else {
		logger.Warn("readiness db probe not configured; /readyz only checks drain flag")
	}
	mux.HandleFunc("/readyz", healthx.Readiness(probes...))

	go func() {
		logger.Info("metrics http listening", zap.String("addr", addr))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}
