// Package metrics exposes Prometheus counters / histograms for card-payment.
//
// 关键指标语义（运维约定）：
//   - paycard_authorize_total{network,result}: 进入 processor.Authorize 的笔数。
//     result=ok|denied|error|timeout，对账「错配」时优先看 ok+denied 与
//     卡组织 settlement 的差。
//   - paycard_authorize_duration_seconds{network}: end-to-end，含 Detokenize +
//     network adapter HTTPS。P99 异常通常是 card-center 链路慢，不是网络。
//   - paycard_network_error_total{network,kind}: 卡组织级错误。kind=
//     timeout|tls|http_5xx|decode|other，能直接喂熔断器决策。
package metrics

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// AuthorizeTotal Authorize 计数（按 network + result）。
// result 取值：ok / denied / error / timeout。
var AuthorizeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycard_authorize_total",
	Help: "Authorize calls by network + result (ok|denied|error|timeout)",
}, []string{"network", "result"})

// AuthorizeDuration Authorize 端到端延迟（含 Detokenize + network adapter）。
// 桶覆盖 10ms ~ 30s：网络 5xx 的 timeout 一般落在 5s/10s/30s 桶。
var AuthorizeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycard_authorize_duration_seconds",
	Help:    "Authorize end-to-end duration (Detokenize + network HTTPS)",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"network"})

// NetworkErrorTotal 卡组织级错误细分。kind:
//
//	timeout    上下文截断 / dial timeout
//	tls        TLS 握手失败 / cert 过期
//	http_5xx   visa/mc 端 5xx
//	decode     响应体不能解析（schema 漂移）
//	other      其余兜底
//
// PromQL: sum by (network) (rate(paycard_network_error_total[5m])) > 0.5
// 触发熔断器降级。
var NetworkErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycard_network_error_total",
	Help: "Card network adapter errors by network + kind",
}, []string{"network", "kind"})

// DetokenizeTotal Detokenize 调用计数（成功 / 失败）。
// 失败率突变常常是 card-center mTLS 链路或 KMS 后端的征兆。
var DetokenizeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycard_detokenize_total",
	Help: "Detokenize gRPC calls to card-center by result",
}, []string{"result"})

// DetokenizeDuration Detokenize 延迟。HSM 后端 P99 5s 内属正常。
var DetokenizeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycard_detokenize_duration_seconds",
	Help:    "Detokenize gRPC end-to-end duration",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
}, []string{"result"})

// GRPCRequestTotal 入站 gRPC 请求计数（method + code）。
var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycard_grpc_request_total",
	Help: "gRPC requests by method + code",
}, []string{"method", "code"})

var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycard_grpc_request_duration_seconds",
	Help:    "gRPC request latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
}, []string{"method"})

// Register 把所有 collector 注册到默认 registry。main.go fx.Invoke 时调一次。
func Register() {
	prometheus.MustRegister(
		AuthorizeTotal,
		AuthorizeDuration,
		NetworkErrorTotal,
		DetokenizeTotal,
		DetokenizeDuration,
		GRPCRequestTotal,
		GRPCRequestDuration,
	)
}

// draining 进程收到 SIGTERM 后置 true，/readyz 立刻返回 503，
// LB / k8s endpoints 下一次探针把本实例摘流量。fx OnStop 调 BeginDrain()。
var draining atomic.Bool

// BeginDrain 标记 drain 状态：/readyz 503。
func BeginDrain() { draining.Store(true) }

// StartServer 起 /metrics + /healthz + /readyz HTTP server。
// 不挂任何 mTLS：仅 prometheus scrape + k8s probe 用，端点应在隔离 DC 内网。
func StartServer(addr string, logger *zap.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	go func() {
		logger.Info("card-payment metrics http listening", zap.String("addr", addr))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}
