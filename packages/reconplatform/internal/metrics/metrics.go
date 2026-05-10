package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Exception counter: total exceptions detected, labeled by type and severity
	// type: "amount_mismatch", "state_mismatch", etc.
	// severity: "critical", "warning", "info"
	ExceptionTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_exception_total",
			Help: "Total exceptions detected in reconciliation",
		},
		[]string{"type", "severity"},
	)

	// Exception pending gauge: current count of unprocessed exceptions
	ExceptionPending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "recon_exception_pending",
			Help: "Current count of pending unprocessed exceptions",
		},
	)

	// Diff amount counter: total amount differences by type
	// type: "amount_mismatch", "state_mismatch", etc.
	DiffAmountMinorTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_diff_amount_minor_total",
			Help: "Total amount differences detected (in minor units, e.g. centavos)",
		},
		[]string{"type"},
	)

	// Run duration histogram: reconciliation run duration in seconds
	RunDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "recon_run_duration_seconds",
			Help:    "Reconciliation run duration in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10},
		},
	)

	// Run error counter: reconciliation run failures
	RunErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "recon_run_errors_total",
			Help: "Total reconciliation run errors",
		},
	)

	// Run total counter: total reconciliation runs
	RunTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "recon_run_total",
			Help: "Total reconciliation runs attempted",
		},
	)

	// ─── archive worker (ClickHouse 冷热分层) ───────────────────────
	// 归档批次结果 — 标签 result: ok|err
	ArchiveBatchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_archive_batch_total",
			Help: "Archive batches sent to ClickHouse (per state)",
		},
		[]string{"state", "result"}, // state=open/acked/...; result=ok/err
	)
	// 单批次归档行数（histogram，看分布）
	ArchiveBatchSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "recon_archive_batch_size",
			Help:    "Rows per archive batch insert",
			Buckets: []float64{1, 10, 50, 100, 200, 500, 1000},
		},
	)
	// 归档延迟（CH INSERT HTTP 来回耗时）
	ArchiveInsertSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "recon_archive_insert_seconds",
			Help:    "ClickHouse INSERT round-trip latency",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10},
		},
	)
	// 跨层 search 计数 — 看流量是不是被冷查走光了（成本指标）
	ArchiveSearchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_archive_search_total",
			Help: "Cross-tier diff search calls",
		},
		[]string{"tier"}, // hot / cold / mixed
	)

	// ─── tracing / Jaeger 跳转 ─────────────────────────────────────
	// 标签 result: hit (有 trace_id) / miss (空) / invalid (sanitize 后为空)
	TracingJaegerLookups = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_tracing_jaeger_lookups_total",
			Help: "/api/v1/diffs/:id/trace endpoint calls",
		},
		[]string{"result"},
	)

	// ─── SSE 实时事件流连接 ─────────────────────────────────────────
	// 当前活跃 SSE 连接数（admin web 打开越多越高）
	SSEActiveConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "recon_sse_active_connections",
			Help: "Active /api/v1/events/stream SSE connections",
		},
	)
	// 推送给客户端的事件数（按 admin 实时观察用量）
	SSEEventsPushed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "recon_sse_events_pushed_total",
			Help: "Events pushed via SSE to clients",
		},
	)

	// ─── DSL 模板 render 计数 — 运营是不是在用 DSL ─────────────────
	DSLRenderTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_dsl_render_total",
			Help: "DSL template renders (template_id label)",
		},
		[]string{"template_id", "result"}, // result=ok/err
	)
)
