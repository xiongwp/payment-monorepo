package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof" // 注册 /debug/pprof/* 到 DefaultServeMux；mux.HandleFunc("/admin/debug/pprof/") forward 过来
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xiongwp/payment-util/healthx"
	"go.uber.org/zap"
)

var ScreenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_screen_total",
	Help: "Risk screen calls by decision (ALLOW/DENY/REVIEW)",
}, []string{"decision"})

var ScreenDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "risk_screen_duration_seconds",
	Help:    "Risk screen latency",
	Buckets: []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1},
}, []string{})

// ScreenStageDuration 给 Screen 各阶段拆分计时，找性能瓶颈。
// stage 取值：feature_extract / ip_intel / ml_score / engine_eval / audit_write
// 让 SRE 看 dashboard 一眼区分 "整体慢是 ML 慢还是 IP intel 慢"。
var ScreenStageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "risk_screen_stage_duration_seconds",
	Help:    "Per-stage Screen latency",
	Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
}, []string{"stage"})

// ScreenSlowTotal 慢请求按最慢 stage 计数，给运营 "p99 突变" 排查用。
var ScreenSlowTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_screen_slow_total",
	Help: "Number of Screen calls exceeding slow threshold, by slowest stage",
}, []string{"slowest_stage"})

// AuditAsyncDropped 给 audit.AsyncBatchSink dropped 计数暴露 Prometheus gauge。
// > 0 持续 = inner sink (CH/Kafka/File) 跟不上 → 调 queueSize / batchSize。
var AuditAsyncDropped = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_audit_async_dropped_total",
	Help: "Audit records dropped by AsyncBatchSink due to inner sink backpressure",
})

// AuditAsyncQueueLen 当前队列深度（gauge）。接近 queueSize 上限 → 接近 drop。
var AuditAsyncQueueLen = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_audit_async_queue_len",
	Help: "Audit AsyncBatchSink current queue depth",
})

// AuditFileSinkSize 当前文件 sink 字节数（接近 maxSize → 即将 rotate）。
var AuditFileSinkSize = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_audit_file_size_bytes",
	Help: "Current size of audit file (rotates at max_size)",
})

// GoroutineCount runtime.NumGoroutine 周期采样。线性增长 = goroutine 泄漏。
// 风控服务正常水位 ~50-200；> 1000 持续 = 有 worker 漏 stop / context.WithCancel
// 没调 cancel / channel reader 没退。
var GoroutineCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_goroutine_count",
	Help: "runtime.NumGoroutine() snapshot",
})

// HeapAllocBytes runtime.MemStats.HeapAlloc。OOM 早期信号。
var HeapAllocBytes = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_heap_alloc_bytes",
	Help: "runtime.MemStats.HeapAlloc snapshot",
})

// HeapInuseBytes 实际占用堆 (alloc + 已分配但未用空闲对象)。
var HeapInuseBytes = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_heap_inuse_bytes",
	Help: "runtime.MemStats.HeapInuse snapshot",
})

// GCCount 累计 GC 次数（不是 rate；Prometheus 端做 rate(...[5m]) 看 GC 频率）。
var GCCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_gc_count_total",
	Help: "runtime.MemStats.NumGC cumulative count",
})

var GRPCRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_grpc_request_total",
}, []string{"method", "code"})

var GRPCRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "risk_grpc_request_duration_seconds",
	Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
}, []string{"method"})

// RuleEvalTotal 按 rule_id × 命中结果计数。
//   - hit="hit"：规则返回 Hit（命中）
//   - hit="miss"：规则返回 nil（放行）
//   - hit="error"：规则 panic / 返回错误（panic 已被 engine 捕获，留给将来可观测）
// 用法：rate(risk_rule_evaluation_total{hit="hit"}[5m]) by (rule_id) → 找出哪些规则在大量命中
var RuleEvalTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_rule_evaluation_total",
	Help: "Per-rule evaluation count",
}, []string{"rule_id", "rule_type", "hit"})

// RuleEvalDuration 按 rule_id 单条规则耗时。桶细到 100us：内存规则（黑名单查 map）
// 几十微秒就出来；外部 API（征信）可能 100ms+。粗桶看不出哪条规则成了瓶颈。
// SyntheticProbeTotal 探针执行计数：result ∈ ok / mismatch / error。
// alert：rate(... mismatch[5m]) > 0 即触发 paging（关键规则突然不 fire 了）。
var SyntheticProbeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_synthetic_probe_total",
	Help: "Synthetic monitoring probes by name + result.",
}, []string{"name", "result"})

var RuleEvalDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "risk_rule_evaluation_duration_seconds",
	Help:    "Per-rule evaluation latency",
	Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
}, []string{"rule_id", "rule_type"})

var once sync.Once

func Register() {
	once.Do(func() {
		prometheus.MustRegister(
			ScreenTotal,
			ScreenDuration,
			ScreenStageDuration,
			ScreenSlowTotal,
			GRPCRequestTotal,
			GRPCRequestDuration,
			RuleEvalTotal,
			RuleEvalDuration,
			SyntheticProbeTotal,
			AuditAsyncDropped,
			AuditAsyncQueueLen,
			AuditFileSinkSize,
			GoroutineCount,
			HeapAllocBytes,
			HeapInuseBytes,
			GCCount,
		)
	})
}

// EngineReadiness 用于 /readyz 的 callback：返回当前已加载的规则数。
// 0 → fail，因为引擎没规则时所有 Screen 都退化成允许，等于没风控。
type EngineReadiness func() int

// AuditQuery 用于 /admin/audit/decisions 端点的 callback：返回最近 limit 条
// 决策审计。nil → 不注册该端点。
type AuditQuery func(limit int) []*AuditRow

// AuditRow 是 audit.DecisionAudit 的最小投影（避免 metrics 包反向依赖
// audit 包）。调用方在 main 里 wrap audit.MemSink.Recent → []*AuditRow。
type AuditRow struct {
	DecisionID     string  `json:"decision_id"`
	OccurredAt     string  `json:"occurred_at"`
	Verdict        string  `json:"verdict"`
	RiskScore      int     `json:"risk_score"`
	RiskLevel      string  `json:"risk_level"`
	HitRules       []string `json:"hit_rules"`
	EvalDurationMs float64 `json:"eval_duration_ms"`
}

// StartServer 起一个 HTTP server 暴露 /metrics + /healthz + /readyz +
// /admin/audit/decisions。
//
// SessionRegisterer 由 main 注入：register Web SDK endpoints（POST
// /v1/risk/session + /finalize）。nil 时不注册。
type SessionRegisterer func(*http.ServeMux)

// engineRules 为 nil 时退到 always-OK readiness（dev / loadtest 入口）。
// auditQ 为 nil 时不注册 /admin/audit/decisions 端点。
// sessionReg 为 nil 时不注册 /v1/risk/session 端点。
//
// 注意：本变体保留兼容，**不带 admin auth**。生产部署用
// StartServerWithAuth 注入 admin token middleware。
func StartServer(addr string, logger *zap.Logger, engineRules EngineReadiness, auditQ AuditQuery, sessionReg SessionRegisterer) {
	StartServerWithAuth(addr, logger, engineRules, auditQ, sessionReg, nil)
}

// StartServerWithAuth 同 StartServer，但 adminAuth 非 nil 时让 /admin/* 路由
// 走 auth middleware；公网路径（/metrics、/healthz、/readyz、/v1/*）不受影响。
// 生产推荐用法：
//
//	src := metrics.NewFileTokenSource("/run/secrets/risk_admin_tokens", 30*time.Second, logger)
//	mw  := metrics.AdminAuthFromSource(src)
//	metrics.StartServerWithAuth(addr, logger, ..., mw)
func StartServerWithAuth(
	addr string, logger *zap.Logger,
	engineRules EngineReadiness, auditQ AuditQuery, sessionReg SessionRegisterer,
	adminAuth func(http.Handler) http.Handler,
) {
	innerMux := http.NewServeMux()
	innerMux.Handle("/metrics", promhttp.Handler())
	innerMux.HandleFunc("/healthz", healthx.Liveness)
	if auditQ != nil {
		innerMux.HandleFunc("/admin/audit/decisions", makeAuditHandler(auditQ, logger))
	}
	if sessionReg != nil {
		sessionReg(innerMux)
	}
	// 仅对 /admin/* 启用 auth 包装；其余直接走 innerMux。
	mux := innerMux
	var rootHandler http.Handler = mux
	if adminAuth != nil {
		rootHandler = pathScopedAuth(mux, "/admin/", adminAuth)
	}

	probes := []healthx.Probe{}
	if engineRules != nil {
		probes = append(probes, healthx.ProbeFunc{
			N: "rule_engine",
			F: func(_ context.Context) error {
				if n := engineRules(); n == 0 {
					return fmt.Errorf("no rules loaded; risk decisions would degrade to allow-all")
				}
				return nil
			},
		})
	} else {
		logger.Warn("readiness rule engine probe not configured; /readyz will always pass")
	}
	mux.HandleFunc("/readyz", healthx.Readiness(probes...))

	// pprof 暴露在 /admin/debug/pprof/* (admin auth + 同端口；公网默认不暴
	// 露)。CPU profile / heap dump / goroutine 等性能 troubleshooting 的
	// 直接入口。生产建议 admin auth 必须开 + 防火墙限内网。
	//
	// import _ "net/http/pprof" 已经把 handler 注册到 DefaultServeMux 的
	// /debug/pprof/*；这里 forward。
	mux.HandleFunc("/admin/debug/pprof/", func(w http.ResponseWriter, r *http.Request) {
		// 重写 path 给 DefaultServeMux 找 pprof handler
		newPath := "/debug/pprof/" + r.URL.Path[len("/admin/debug/pprof/"):]
		r.URL.Path = newPath
		http.DefaultServeMux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 5 * time.Second, // 防 slowloris
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	currentAdminServer.Store(srv)
	go func() {
		logger.Info("metrics http listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics http stopped", zap.Error(err))
		}
	}()
}

// currentAdminServer 进程级 *http.Server 单例引用，给 ShutdownAdmin
// 调 Shutdown(ctx) 让 in-flight HTTP 请求跑完。fx OnStop hook 调。
var currentAdminServer atomic.Pointer[http.Server]

// ShutdownAdmin graceful shutdown：等 in-flight HTTP 请求跑完，超
// timeout 强制关。fx Lifecycle OnStop 钩子里调。
func ShutdownAdmin(ctx context.Context, timeout time.Duration) error {
	srv := currentAdminServer.Load()
	if srv == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	shutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// pathScopedAuth 把 auth middleware 仅作用在 prefix 命中的 path 上；其他路径
// 直接 ServeHTTP next（即 mux）。
func pathScopedAuth(next http.Handler, prefix string, auth func(http.Handler) http.Handler) http.Handler {
	guarded := auth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= len(prefix) && r.URL.Path[:len(prefix)] == prefix {
			guarded.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// makeAuditHandler GET /admin/audit/decisions?limit=N (默认 100，最多 1000)。
// 返回最近 N 条决策审计 JSON 数组。仅作为运营 / SRE 临时排查用，不是
// 持久化审计的最终来源（生产应配 Kafka / DB 长留存 sink）。
func makeAuditHandler(q AuditQuery, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 1000 {
			limit = 1000
		}
		rows := q(limit)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			logger.Warn("audit decisions: encode failed", zap.Error(err))
		}
	}
}
