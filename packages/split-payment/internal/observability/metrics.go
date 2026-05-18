// Package observability — split-payment 的 Prometheus metric 集中注册点 + 暴露 helper.
//
// SP-AC-7 P9: 给 split-payment 补全 metric — 之前是 0 metric, 生产无法定位瓶颈.
//
// 设计:
//   - 用 promauto 自动注册到默认 DefaultRegisterer; main 里只需要 import 这个包就生效.
//   - 4 个核心 metric 覆盖：trigger 延迟 + 各 voucher 状态计数 + accounting RPC 延迟 + saga 失败计数.
//   - HTTP /metrics 由 promhttp 提供, 跟 /healthz 共用一个 admin HTTP server (见 admin.go).
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// TriggerDuration — TriggerEvent 端到端耗时 (含 translate + 所有 accounting RPC).
// labels: graph_key, outcome ("success" / "partial" / "failed")
var TriggerDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "split_payment",
	Name:      "trigger_duration_seconds",
	Help:      "TriggerEvent end-to-end latency (translate + all CreateTransaction calls).",
	Buckets:   prometheus.DefBuckets, // .005 to 10s
}, []string{"graph_key", "outcome"})

// SaveGraphDuration — SaveGraph saga 耗时 (含 rule push 到 accounting + 本地 INSERT).
var SaveGraphDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "split_payment",
	Name:      "save_graph_duration_seconds",
	Help:      "SaveGraph saga latency (rule sync + local DB write).",
	Buckets:   prometheus.DefBuckets,
}, []string{"graph_key", "outcome"})

// AccountingRPCDuration — split-payment → accounting gRPC 单次调用耗时.
// labels: method ("CreateTransaction" / "ResetOrder" / "UpsertRules"), code ("ok"/"error")
var AccountingRPCDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "split_payment",
	Name:      "accounting_rpc_duration_seconds",
	Help:      "Latency of RPC calls from split-payment to accounting-system.",
	Buckets:   prometheus.DefBuckets,
}, []string{"method", "code"})

// VoucherStatusCount — 每个 TriggerEvent 返回的 voucher 按 status 计数.
// labels: event_code, status ("success"=2 / "failed"=3 / "processing"=1 / "pending"=0)
var VoucherStatusCount = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "split_payment",
	Name:      "voucher_status_total",
	Help:      "Count of vouchers returned by TriggerEvent, grouped by event_code + final status.",
}, []string{"event_code", "status"})

// SagaStateCount — saga 各状态 gauge (snapshotted by background poller).
var SagaStateCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "split_payment",
	Name:      "saga_state_count",
	Help:      "Current count of saga rows by state (forwarding / completed / compensating / failed).",
}, []string{"state"})

// PayoutQueueDepth — 待 dispatch payout 数量.
var PayoutQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "split_payment",
	Name:      "payout_queue_depth",
	Help:      "Number of payouts in status=pending awaiting dispatch.",
})

// RefundEventCount — Kafka refund event 消费计数.
var RefundEventCount = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "split_payment",
	Name:      "refund_event_total",
	Help:      "Refund Kafka events consumed by outcome.",
}, []string{"outcome"}) // "ok" / "parse_error" / "handle_error"

// VoucherStatusLabel 给 int8 status 转可读 string, 让 metric label 不暴露 magic number.
func VoucherStatusLabel(s int8) string {
	switch s {
	case 0:
		return "pending"
	case 1:
		return "processing"
	case 2:
		return "success"
	case 3:
		return "failed"
	default:
		return "unknown"
	}
}
