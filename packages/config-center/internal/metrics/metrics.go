// Package metrics 暴露 config-center prometheus collectors。
//
// 命名约定：configcenter_* 前缀（跟 paycard_ / paycore_ 一致）。
package metrics

import "github.com/prometheus/client_golang/prometheus"

// PushTotal admin write 后通知 watcher 的次数。
//   strategy: FULL / CANARY / TARGETED / SCHEDULED
// 用 PromQL: sum by (strategy) (rate(configcenter_push_total[5m]))
// 看哪种策略推送占比，flagging 灰度配置长期不收尾。
var PushTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "configcenter_push_total",
	Help: "Configs pushed via service.hub by strategy",
}, []string{"strategy"})

// PushMatchTotal 单 watcher 收到事件后命中策略的结果。
//   result: matched / filtered_out
// 灰度推送时大部分事件会被 filter；正常的。这个 metric 看灰度命中率。
var PushMatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "configcenter_push_match_total",
	Help: "Per-watcher event filtering result",
}, []string{"strategy", "result"})

// SubscribersGauge 当前订阅 watcher 数（per namespace）。
// scrape 时实时计数；> N 时考虑增加 server 副本。
var SubscribersGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "configcenter_subscribers",
	Help: "Current watch streams per namespace",
}, []string{"namespace"})

// PutTotal admin 写入次数（per op + actor）。
var PutTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "configcenter_put_total",
	Help: "Admin writes by op (PUT/ROLLBACK/DELETE)",
}, []string{"op"})

// SnapshotInitDuration SDK 启动时拉初始 snapshot 的延迟。
// > 5s 是异常，看 server 端 SinceVersion / ListNamespaceForSnapshot 慢查询。
var SnapshotInitDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "configcenter_snapshot_init_duration_seconds",
	Help:    "Server-side latency to ship initial snapshot to a new watcher",
	Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
}, []string{"namespace"})

// PendingGauge 当前 server-side pending（effective_at > now）的 config 数量。
// > 0 = 有定时配置在排队；> N 阈值时 admin UI 黄字提示。
var PendingGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "configcenter_pending",
	Help: "Configs scheduled for future activation (effective_at > now)",
}, []string{"namespace"})

// Register 启动期一次性注册。
func Register() {
	prometheus.MustRegister(
		PushTotal,
		PushMatchTotal,
		SubscribersGauge,
		PutTotal,
		SnapshotInitDuration,
		PendingGauge,
	)
}
