// Package metrics exposes Prometheus counters / histograms for order-core.
package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	_ "net/http/pprof" // ROI-2e: pprof handlers on http.DefaultServeMux
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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

// MerchantRateLimitThrottled merchant 维度限流触发计数。告警规则：单 merchant 5min 内 > 1000。
var MerchantRateLimitThrottled = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "order_merchant_ratelimit_throttled_total",
	Help: "Requests throttled by merchant rate limit",
}, []string{"merchant_id"})

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

// AcctOutboxPendingGauge 当前 pending 状态的 outbox 行数（积压量）。
// 每个 Tick 后由 worker 更新；持续上涨说明下游 accounting-system 处理不过来。
var AcctOutboxPendingGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "order_acct_outbox_pending_count",
	Help: "Current pending accounting outbox rows (lag indicator)",
})

// AcctOutboxOldestPendingAgeSeconds 当前 pending 队列里最老的那条的年龄（秒）。
// 比 pending_count 更直接反映 lag。SLO：P99 < 30s；P0 告警：> 5min。
var AcctOutboxOldestPendingAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "order_acct_outbox_oldest_pending_age_seconds",
	Help: "Age (seconds) of the oldest pending accounting outbox row",
})

// ─── DB 连接池 ────────────────────────────────────────────────────────────────
//
// 由 repo.Manager 在启动期 spawn 一个 goroutine 周期采样 sql.DB.Stats() 写入。
// 关键告警：
//   - db_pool_in_use_count / max_open 比例 > 80% 持续 5min → 即将打爆
//   - db_pool_wait_count 增速 > 100/min → 已经在排队，下一步雪崩
//   - db_pool_max_idle_closed 增速高 → MaxIdleConns 太小，连接频繁重建

// DBPoolMaxOpen sql.DB.MaxOpenConnections（配置上限）
var DBPoolMaxOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_max_open",
	Help: "Configured max open connections per DB",
}, []string{"db"})

// DBPoolOpen 当前 open 总数（in_use + idle）
var DBPoolOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_open_count",
	Help: "Current open connections (in_use + idle)",
}, []string{"db"})

// DBPoolInUse 当前正在被某个 goroutine 持有的连接数
var DBPoolInUse = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_in_use_count",
	Help: "Connections currently checked out by callers",
}, []string{"db"})

// DBPoolIdle 当前空闲池里的连接数
var DBPoolIdle = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_idle_count",
	Help: "Idle connections in the pool",
}, []string{"db"})

// DBPoolWaitCount 累计等待获取连接的次数（counter，单调递增）
var DBPoolWaitCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_wait_count_total",
	Help: "Cumulative count of connection waits (rate => contention)",
}, []string{"db"})

// DBPoolWaitDuration 累计等待时长（秒）
var DBPoolWaitDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_wait_duration_seconds_total",
	Help: "Cumulative seconds blocked waiting for a connection",
}, []string{"db"})

// DBPoolMaxIdleClosed 因 MaxIdleConns 限制被关闭的连接数（counter）
var DBPoolMaxIdleClosed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_max_idle_closed_total",
	Help: "Connections closed due to MaxIdleConns limit (raise MaxIdleConns if growing fast)",
}, []string{"db"})

// DBPoolMaxLifetimeClosed 因 ConnMaxLifetime 限制被关闭的连接数（counter）
var DBPoolMaxLifetimeClosed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "order_db_pool_max_lifetime_closed_total",
	Help: "Connections closed due to ConnMaxLifetime",
}, []string{"db"})

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
		MerchantRateLimitThrottled,
		WebhookDeliveryTotal,
		WebhookDeliveryLatency,
		WorkerRunTotal,
		StuckProcessingGauge,
		AcctOutboxProcessTotal,
		AcctOutboxBatchSize,
		AcctOutboxTickInterval,
		AcctOutboxPendingGauge,
		AcctOutboxOldestPendingAgeSeconds,
		DBPoolMaxOpen,
		DBPoolOpen,
		DBPoolInUse,
		DBPoolIdle,
		DBPoolWaitCount,
		DBPoolWaitDuration,
		DBPoolMaxIdleClosed,
		DBPoolMaxLifetimeClosed,
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
	StartServerWithLevel(addr, logger, nil, probes...)
}

// StartServerWithLevel 同 StartServer 但加 /debug/pprof + /admin/log-level.
//
// ROI-2e: 与 split-payment / payment-core / payment-channel / api-gateway / card-payment 一致.
// level=nil 时 /admin/log-level 端点不挂.
func StartServerWithLevel(addr string, logger *zap.Logger, level *zap.AtomicLevel, probes ...HealthProbe) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		runReadyProbes(r.Context(), w, probes)
	})
	// ROI-2e: pprof.
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	// ROI-2e: log level.
	if level != nil {
		mux.HandleFunc("/admin/log-level", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"level": level.Level().String()})
			case http.MethodPut, http.MethodPost:
				var body struct{ Level string `json:"level"` }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "bad json", http.StatusBadRequest)
					return
				}
				var l zapcore.Level
				if err := l.UnmarshalText([]byte(body.Level)); err != nil {
					http.Error(w, "bad level", http.StatusBadRequest)
					return
				}
				level.SetLevel(l)
				logger.Info("log level changed", zap.String("level", body.Level))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"level": l.String()})
			default:
				http.Error(w, "method", http.StatusMethodNotAllowed)
			}
		})
	}
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
