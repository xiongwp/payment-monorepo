// Package metrics exposes Prometheus counters / histograms for payment-core.
package metrics

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// BreakerRegistry is the minimal surface the metrics HTTP server needs from
// circuitbreaker.Registry. Decoupled via interface so we don't drag the
// breaker package into metrics.
type BreakerRegistry interface {
	States() map[string]string
	Reset(adapter string) bool
	ResetAll() int
}

var RouteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_route_total",
	Help: "Routing decisions by payment_method + adapter (or no_match)",
}, []string{"payment_method", "adapter"})

var ChargeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_charge_total",
	Help: "Charge calls by payment_method + adapter + result",
}, []string{"payment_method", "adapter", "result"})

var ChargeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycore_charge_duration_seconds",
	Help:    "Charge end-to-end duration (including payment-channel RPC)",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"payment_method", "adapter"})

var RefundTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_refund_total",
	Help: "Refund calls by adapter + result",
}, []string{"adapter", "result"})

var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_grpc_request_total",
	Help: "gRPC requests by method + code",
}, []string{"method", "code"})

var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycore_grpc_request_duration_seconds",
	Help:    "gRPC request latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"method"})

// MerchantRateLimitThrottled merchant 维度限流触发计数。告警规则：单 merchant 5min 内 > 1000。
var MerchantRateLimitThrottled = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_merchant_ratelimit_throttled_total",
	Help: "Requests throttled by merchant rate limit",
}, []string{"merchant_id"})

// CircuitState 每个 adapter 当前熔断状态：0=Closed 1=Open 2=HalfOpen。
// Gauge 让 PromQL 可以直接判断「现在多少个 adapter 处于 Open」（sum by (state)）。
var CircuitState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "paycore_circuit_state",
	Help: "Current circuit breaker state per adapter (0=closed 1=open 2=half_open)",
}, []string{"adapter"})

// CircuitTransitions 状态切换事件。flapping 检测：
// rate(paycore_circuit_transitions_total{from="CLOSED",to="OPEN"}[5m]) 频繁就是抖动。
var CircuitTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_circuit_transitions_total",
	Help: "Circuit breaker state transitions per adapter",
}, []string{"adapter", "from", "to"})

// RiskScreenTotal 风控决策计数：fail-open / fail-close / allow / deny / review。
// fail-open 路径常驻数值不为 0 是设计意图（risk 慢/挂时让交易先通过），
// 但增长率显著抬升时需要排查 risk-manage 健康度。
var RiskScreenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_risk_screen_total",
	Help: "Risk screen calls by outcome (allow / deny / review / fail_open / fail_close)",
}, []string{"outcome"})

// RiskScreenDuration risk Screen 端到端耗时（含 ctx 超时被截断的部分）。
// 桶刻意细到 1ms：高流量商户的 per-merchant timeout 可能配到 100~500ms，
// 再粗的桶看不出尾延优化效果。
var RiskScreenDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "paycore_risk_screen_duration_seconds",
	Help:    "Risk Screen latency end-to-end",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"outcome"})

// RiskReportTotal 交易结果回报 risk-manage 计数（统计后置，与 Charge 结果分开）。
// status: ok / error / dropped(因 ctx 已 done 跳过)
var RiskReportTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "paycore_risk_report_total",
	Help: "Post-charge risk.Report calls by status",
}, []string{"status"})

// P0-3：备用渠道路由指标
var RoutingFallbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "payment_routing_fallback_total",
	Help: "Fallback routing decisions from primary to secondary adapter",
}, []string{"from_state", "to_adapter"})

// P0-3：重试队列入队指标
var RoutingRetryEnqueuedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "payment_routing_retry_enqueued_total",
	Help: "Charge tasks enqueued for async retry (circuit_open / unavailable)",
}, []string{"reason"})

func Register() {
	prometheus.MustRegister(
		RouteTotal,
		ChargeTotal,
		ChargeDuration,
		RefundTotal,
		GRPCRequestTotal,
		GRPCRequestDuration,
		MerchantRateLimitThrottled,
		CircuitState,
		CircuitTransitions,
		RiskScreenTotal,
		RiskScreenDuration,
		RiskReportTotal,
		RoutingFallbackTotal,
		RoutingRetryEnqueuedTotal,
	)
}

// draining 进程收到 SIGTERM 后置 true，/readyz 立刻返回 503，K8s / LB
// 下一次探针把本实例从 endpoints 里摘掉。fx OnStop hook 调 BeginDrain()。
var draining atomic.Bool

// BeginDrain 标记进入 drain 状态：/readyz 返回 503。
func BeginDrain() { draining.Store(true) }

func StartServer(addr string, logger *zap.Logger, breakers BreakerRegistry) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// readiness 探针：drain 时返回 503 摘流量，正常时 200。
	// k8s readinessProbe 走这个端点，不应鉴权（探针不带 header）。
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	// Ops surface for circuit breakers — lets the admin UI reset a stuck
	// adapter without restarting the service (e.g. after a QA probe batch).
	if breakers != nil {
		mux.HandleFunc("/ops/circuit/states", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(breakers.States())
		})
		mux.HandleFunc("/ops/circuit/reset", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			adapter := r.URL.Query().Get("adapter")
			w.Header().Set("Content-Type", "application/json")
			if adapter == "" {
				n := breakers.ResetAll()
				_ = json.NewEncoder(w).Encode(map[string]any{"reset": n, "all": true})
				return
			}
			ok := breakers.Reset(adapter)
			_ = json.NewEncoder(w).Encode(map[string]any{"reset": ok, "adapter": adapter})
		})
	}
	go func() {
		logger.Info("metrics http listening", zap.String("addr", addr))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}
