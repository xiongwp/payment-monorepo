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
	"net/http/pprof"
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

// CircuitState 每个 network adapter 的熔断器状态。0=Closed 1=Open 2=HalfOpen。
// PromQL: max by (network) (paycard_circuit_state) > 0 → 至少一个 network 不 Closed。
var CircuitState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "paycard_circuit_state",
	Help: "Network adapter circuit breaker state (0=closed 1=open 2=half_open)",
}, []string{"network"})

// CircuitTransitions 状态切换计数。flapping 检测：
// rate(paycard_circuit_transitions_total{from=\"CLOSED\",to=\"OPEN\"}[5m]) 频繁 → 抖动告警。
var CircuitTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycard_circuit_transitions_total",
	Help: "Network adapter circuit breaker transitions",
}, []string{"network", "from", "to"})

// DeclineCategoryTotal 卡组织 decline 分类计数。HARD = 黑名单候选，
// 业务监控应该关注 HARD 增长率（突发 = 卡组织风控规则变了 / 真有 attack）。
var DeclineCategoryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycard_decline_category_total",
	Help: "Authorize decline by network + category (HARD/SOFT)",
}, []string{"network", "category"})

// FraudScoreHist 卡组织端反欺诈分布（HARD decline 时一般 90+，正常 < 30）。
// 桶按 10 分一档，便于 PromQL histogram_quantile。
var FraudScoreHist = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycard_fraud_score",
	Help:    "Network-returned fraud score distribution per network",
	Buckets: []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100},
}, []string{"network"})

// HardDeclineRate 监控 HARD 占比；用 sum_over_time 计算 5min 窗内 HARD/total。
// （Counter 不直接给 rate；这里靠 PromQL 算）

// BulkheadActive 当前在飞的 Authorize 总数（per-instance）。saturation 信号。
// PromQL: paycard_bulkhead_active / paycard_bulkhead_capacity > 0.8 → 报警
var BulkheadActive = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "paycard_bulkhead_active",
	Help: "Authorize calls currently in flight (sum across all merchants)",
})

var BulkheadRejectedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "paycard_bulkhead_rejected_total",
	Help: "Authorize calls rejected by bulkhead (single-merchant concurrency limit hit)",
})

var BulkheadCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "paycard_bulkhead_capacity",
	Help: "Per-merchant max concurrent Authorize",
})

// DependencyUp 下游依赖可达性（card-center / kms-manage / DB / 各 network endpoint）。
// 1 = up；0 = down。Probe goroutine 每 N 秒刷一次。
// /readyz 把所有 critical=true 的 DependencyUp == 1 才算 ready。
var DependencyUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "paycard_dependency_up",
	Help: "Downstream dependency reachability (1=up, 0=down)",
}, []string{"dependency"})

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
		CircuitState,
		CircuitTransitions,
		DeclineCategoryTotal,
		FraudScoreHist,
		BulkheadActive,
		BulkheadRejectedTotal,
		BulkheadCapacity,
		DependencyUp,
	)
}

// draining 进程收到 SIGTERM 后置 true，/readyz 立刻返回 503，
// LB / k8s endpoints 下一次探针把本实例摘流量。fx OnStop 调 BeginDrain()。
var draining atomic.Bool

// BeginDrain 标记 drain 状态：/readyz 503。
func BeginDrain() { draining.Store(true) }

// ReadinessProbe 由 caller 提供 critical 依赖检查；返 nil = ready。
// 例：返 errors.New("card-center unreachable") 让 K8s 摘流量。
type ReadinessProbe func() error

// StartServer 起 /metrics + /healthz + /readyz HTTP server。
// 不挂任何 mTLS：仅 prometheus scrape + k8s probe 用，端点应在隔离 DC 内网。
//
// probe nil → /readyz 仅看 drain 状态（向后兼容）。
// probe 非空 → 失败时 503 + body 写错误原因（运维 tail 看）。
func StartServer(addr string, logger *zap.Logger, probe ...ReadinessProbe) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	// pprof：性能调试 / 容量规划必备。生产 metrics 端口走内网，无外暴；
	// CPU / heap / goroutine / mutex / block profile 全开。
	// 用法：
	//   go tool pprof http://card-payment:9544/debug/pprof/profile?seconds=30  (CPU 30s)
	//   go tool pprof http://card-payment:9544/debug/pprof/heap                (heap)
	//   curl http://card-payment:9544/debug/pprof/goroutine?debug=1            (goroutine dump)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// liveness：只要进程没卡死就返 200。drain 也返 200（liveness != readiness）。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		for _, p := range probe {
			if p == nil {
				continue
			}
			if err := p(); err != nil {
				http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
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
