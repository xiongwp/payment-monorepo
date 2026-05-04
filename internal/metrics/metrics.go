// Package metrics exposes Prometheus counters / histograms for order-core.
package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// ─── PaymentIntent ────────────────────────────────────────────────────────────

// PICreatedTotal Create 调用次数
var PICreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_payment_intent_created_total",
	Help: "Total PaymentIntent.Create calls",
}, []string{"livemode", "currency"})

// PIStatusTransitionTotal PI 状态流转计数
var PIStatusTransitionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_payment_intent_transition_total",
	Help: "PaymentIntent status transitions",
}, []string{"from", "to"})

// PIConfirmDuration Confirm 总耗时
var PIConfirmDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "order_payment_intent_confirm_duration_seconds",
	Help:    "Confirm handler duration (creates charge + calls channel + writes back)",
	Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
})

// ─── Channel ─────────────────────────────────────────────────────────────────

// ChannelChargeTotal 渠道 Charge 调用结果
var ChannelChargeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_channel_charge_total",
	Help: "Calls to PaymentChannel.Charge by channel name + result",
}, []string{"channel", "result"})

// ChannelRefundTotal 渠道 Refund 调用结果
var ChannelRefundTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_channel_refund_total",
	Help: "Calls to PaymentChannel.Refund by channel name + result",
}, []string{"channel", "result"})

// ChannelDuration 渠道 RPC 耗时
var ChannelDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "order_channel_rpc_duration_seconds",
	Help:    "PaymentChannel RPC call latency",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"channel", "method"})

// ─── Refund ──────────────────────────────────────────────────────────────────

// RefundCreatedTotal
var RefundCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_refund_created_total",
	Help: "Refund creations by reason + auto_compensate",
}, []string{"reason", "auto_compensate"})

// RefundRetryTotal
var RefundRetryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_refund_retry_total",
	Help: "Refund retry attempts by result",
}, []string{"result"})

// ─── Notify ──────────────────────────────────────────────────────────────────

// NotifyTotal 通知发送结果
var NotifyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_notify_total",
	Help: "Notify attempts by channel + result",
}, []string{"channel", "result"})

// NotifyLatency 通知耗时
var NotifyLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "order_notify_latency_seconds",
	Help:    "Notify dispatch latency",
	Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"channel"})

// ─── gRPC ────────────────────────────────────────────────────────────────────

// GRPCRequestTotal gRPC 请求计数
var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_grpc_request_total",
	Help: "gRPC requests by method + code",
}, []string{"method", "code"})

// GRPCRequestDuration gRPC 请求耗时
var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "order_grpc_request_duration_seconds",
	Help:    "gRPC request latency",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
}, []string{"method"})

// ─── Webhook 投递 ─────────────────────────────────────────────────────────────

var WebhookDeliveryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_webhook_delivery_total",
	Help: "Webhook deliveries by status (succeeded/failed/exhausted)",
}, []string{"status"})

var WebhookDeliveryLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "order_webhook_delivery_latency_seconds",
	Help:    "Webhook HTTP delivery latency (per attempt)",
	Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10},
})

// ─── Worker ───────────────────────────────────────────────────────────────────

var WorkerRunTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_worker_run_total",
	Help: "Background worker execution count",
}, []string{"worker", "result"})

var StuckProcessingGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "order_stuck_processing_count",
	Help: "Number of PIs in PROCESSING state for > 5 minutes (set by ReconcileWorker)",
})

// AcctOutboxProcessTotal accounting outbox 单行处理结果分布。
//   result: ok / retry / exhausted
// rate(...{result="exhausted"}[5m]) 增长意味着 accounting-system 长期不可达，
// 需要人工介入；result="retry" 持续高位说明下游不健康。
var AcctOutboxProcessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_acct_outbox_process_total",
	Help: "Accounting outbox per-row processing outcome",
}, []string{"result"})

// AcctOutboxBatchSize 每次 Tick 抓到的 due 行数。
// 持续接近 batchSize 上限说明 worker 跟不上写入速度，需要扩容或调大 batch。
var AcctOutboxBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "order_acct_outbox_batch_size",
	Help:    "Rows fetched per Tick (0 = empty)",
	Buckets: []float64{0, 1, 5, 10, 25, 50, 100, 250, 500},
})

// AcctOutboxTickInterval worker 当前轮询间隔（自适应：满载快、空载慢）。
// Gauge 值反映自适应退避是否生效；持续高位意味长时间无积压。
var AcctOutboxTickInterval = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "order_acct_outbox_tick_interval_seconds",
	Help: "Current adaptive poll interval of accounting outbox worker",
})

// Register 把所有指标注册到默认 registry（在 main 里调用一次）
func Register() {
	prometheus.MustRegister(
		PICreatedTotal,
		PIStatusTransitionTotal,
		PIConfirmDuration,
		ChannelChargeTotal,
		ChannelRefundTotal,
		ChannelDuration,
		RefundCreatedTotal,
		RefundRetryTotal,
		NotifyTotal,
		NotifyLatency,
		GRPCRequestTotal,
		GRPCRequestDuration,
		WebhookDeliveryTotal,
		WebhookDeliveryLatency,
		WorkerRunTotal,
		StuckProcessingGauge,
		AcctOutboxProcessTotal,
		AcctOutboxBatchSize,
		AcctOutboxTickInterval,
	)
}

// HealthProbe lets StartServer reach into live dependencies (DB pools, gRPC
// clients) to produce a truthful /healthz response. Nil-safe: if probes is
// nil, /healthz stays a trivial "ok".
//
// Each probe returns (name, ok, detail). "detail" appears in the JSON body
// when !ok, so ops can diagnose without tailing logs.
type HealthProbe func() (name string, ok bool, detail string)

// StartServer 启动 /metrics + /healthz + /readyz HTTP（在 goroutine 中，非阻塞）
//
//   /healthz: "alive" — returns 200 as long as the process is running. Used by
//             K8s liveness. Does NOT check dependencies; a DB outage should
//             NOT restart the pod.
//   /readyz : "can serve traffic" — runs every probe; returns 503 if any
//             probe reports !ok. Used by K8s readiness + load-balancer drain.
func StartServer(addr string, logger *zap.Logger, probes ...HealthProbe) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		runReadyProbes(r.Context(), w, probes)
	})
	go func() {
		logger.Info("metrics http listening", zap.String("addr", addr))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}

// runReadyProbes runs each probe with a shared 2-second budget and emits a
// JSON summary. Broken out so callers can test the HTTP contract without a
// real http server.
func runReadyProbes(ctx context.Context, w http.ResponseWriter, probes []HealthProbe) {
	type result struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
	}
	out := struct {
		Status string   `json:"status"`
		Checks []result `json:"checks"`
	}{Status: "ready", Checks: make([]result, 0, len(probes))}
	allOK := true

	done := make(chan struct{})
	go func() {
		for _, p := range probes {
			if p == nil {
				continue
			}
			n, ok, d := p()
			if !ok {
				allOK = false
			}
			out.Checks = append(out.Checks, result{Name: n, OK: ok, Detail: d})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		out.Status = "timeout"
		allOK = false
	case <-ctx.Done():
		return
	}
	if !allOK {
		out.Status = "not_ready"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}
