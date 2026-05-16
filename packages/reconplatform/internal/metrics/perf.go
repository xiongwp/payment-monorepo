// perf.go — 全链路性能指标 (按阶段 histogram).
//
// 设计:
//   - 每个关键阶段独立 histogram → 一眼定位瓶颈是哪段
//   - bucket 用 ExponentialBucketsRange (0.1ms ~ 30s) 覆盖快+慢两种 case
//   - 标签控制基数: 不打 svc/table (基数爆炸),只打"阶段" + 命中/未命中
//
// 抓:
//   curl http://localhost:9180/metrics | grep recon_perf_
//
// 典型告警查询 (Prometheus):
//   histogram_quantile(0.99, sum by (le, stage) (rate(recon_perf_stage_duration_seconds_bucket[5m])))
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PerfStageDuration 单阶段耗时 histogram (秒).
//
// label "stage" 取值:
//   compile          — Starlark 编译 (~ 5-50ms, 命中 cache ~ 0)
//   fetch_candidates — Redis HGETALL 拉桶事件 (~ 0.5-5ms)
//   engine_run       — 执行 starlark.Call(check, ctx) (~ 1-100ms 看脚本复杂度)
//   match_rules      — Registry.EvalAll 跑全部规则 (~ N × engine_run)
//   publish          — KafkaPublisher.Publish 发结果 (~ 0.1-2ms)
//   sse_xread        — XREAD 阻塞 + 解析 (~ 0-5000ms 含 block)
//   candidate_put    — candidate.Layer.Put 1 RTT Lua (~ 0.1-1ms)
//
// label "result": ok / err / cache_hit / cache_miss (只 compile 用 cache_*)
var PerfStageDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "recon",
	Subsystem: "perf",
	Name:      "stage_duration_seconds",
	Help:      "Duration of each performance-critical stage by name and result.",
	// 0.1ms 到 30s,覆盖 cache 命中到极慢规则
	Buckets: prometheus.ExponentialBucketsRange(0.0001, 30, 15),
}, []string{"stage", "result"})

// PerfStageThroughput 各阶段每秒处理事件 / record / trigger 数.
//
// label "stage": ingester / matcher / publisher / cdc_bridge
var PerfStageThroughput = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recon",
	Subsystem: "perf",
	Name:      "stage_throughput_total",
	Help:      "Total events / triggers processed by stage.",
}, []string{"stage"})

// CompileCacheGauge 编译 cache 当前大小 / 命中率.
var CompileCacheGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "recon",
	Subsystem: "perf",
	Name:      "compile_cache",
	Help:      "Starlark compile cache size / hit_rate.",
}, []string{"metric"}) // metric: size / max / hit_rate

// SSEHubGauge SSE 广播 hub 状态: 订阅者数 / 投递 / 丢弃.
var SSEHubGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "recon",
	Subsystem: "perf",
	Name:      "sse_hub",
	Help:      "SSE broadcast hub state (subscribers / delivered / dropped).",
}, []string{"metric"}) // metric: subscribers / produced / delivered / dropped

// CandidatePutLatency 候选层 Put 单次操作耗时分布 — 区分有 / 无 trigger.
//
// 单独 histogram 因为 candidate_put 是最热的路径,要看 P99 (10K events/s 下).
var CandidatePutLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "recon",
	Subsystem: "perf",
	Name:      "candidate_put_seconds",
	Help:      "Candidate layer Put latency. trigger=1 表示本次 Put 触发了 trigger.",
	Buckets:   prometheus.ExponentialBucketsRange(0.0001, 1, 12),
}, []string{"trigger"})

// MatcherWorkerLatency Matcher worker 单 trigger 处理耗时 (Lock+Get+Eval+Ack 总和).
var MatcherWorkerLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "recon",
	Subsystem: "perf",
	Name:      "matcher_trigger_seconds",
	Help:      "Matcher worker total trigger processing time.",
	Buckets:   prometheus.ExponentialBucketsRange(0.0001, 30, 12),
}, []string{"verdict"})

// ─── helpers ───────────────────────────────────────────────────

// ObserveStage 简洁包装: `defer metrics.ObserveStage("compile", "cache_hit", time.Now())()`.
// 返回值是 func,defer 调用时计算并 observe 耗时,几行代码搞定.
//
// 用法:
//
//	func DoCompile() {
//	    done := ObserveStage("compile", "")
//	    defer func() { done("ok") }()
//	    // ... compile ...
//	}
//
// 或更简洁的内联:
//
//	defer ObserveStage("compile", "ok")()
type observeStageFn func(result string)

func ObserveStage(stage, defaultResult string) observeStageFn {
	start := nowTime()
	return func(result string) {
		if result == "" {
			result = defaultResult
		}
		PerfStageDuration.WithLabelValues(stage, result).Observe(secondsSince(start))
	}
}

// nowTime / secondsSince 抽出避免 time 依赖污染:用 stdlib time 即可.
//
//go:noinline
func nowTime() int64 { return timeNowUnixNano() }

func secondsSince(startNs int64) float64 {
	return float64(timeNowUnixNano()-startNs) / 1e9
}
