package metrics

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xiongwp/payment-util/healthx"
	"go.uber.org/zap"
)

// ─── 记账路径 ─────────────────────────────────────────────────────────────────

// BookingDuration 端到端记账延迟直方图（从 DoubleEntryBooking 入口到返回）。
// p99 直接反映用户感知延迟 — 比三段 TCC histogram 相加更精准（percentile 不可加）。
var BookingDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "accounting_booking_duration_seconds",
		Help:    "End-to-end DoubleEntryBooking duration including validation, TCC, DB writes",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
	},
)

// BookingTotal 记账请求总数，按执行路径区分
// path: "hot"（Redis热路径）| "tcc"（MySQL TCC冷路径）
var BookingTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "accounting_booking_total",
		Help: "Total double-entry booking requests, labeled by execution path (hot|tcc)",
	},
	[]string{"path"},
)

// WarmAccountsTotal 缓存 miss 触发预热的次数（雷鸣效应监控）
var WarmAccountsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_warm_accounts_total",
		Help: "Total cache-miss warm-up triggers for hot-path accounts",
	},
)

// WarmAccountFailuresTotal 启动时 hot-account 预热失败次数。
// 非零意味着首批 hot path 请求会 cache miss，最坏情况触发 stampede；
// 持续非零需要立即排查 Redis / DB 连通性。
var WarmAccountFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_warm_account_failures_total",
		Help: "Number of hot accounts that failed to be pre-warmed into Redis at startup",
	},
)

// HotPathFallbackTotal 热路径降级到 TCC 冷路径的次数（Redis 故障或余额不足除外，单独为 cache miss）
// reason: "cache_miss" | "redis_error" | "insufficient_balance"
var HotPathFallbackTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "accounting_hot_path_fallback_total",
		Help: "Total hot-path requests that fell back; non-zero indicates cache or Redis health degradation",
	},
	[]string{"reason"},
)

// ─── Outbox 处理 ──────────────────────────────────────────────────────────────

// OutboxProcessedTotal outbox 成功持久化到 MySQL 的记录总数
var OutboxProcessedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_outbox_processed_total",
		Help: "Total outbox records successfully persisted to MySQL (MYSQL_DONE)",
	},
)

// OutboxFailedTotal outbox 达到重试上限被标记 FAILED 的记录总数
// 此值非零代表需要人工介入的资金安全事件
var OutboxFailedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_outbox_failed_total",
		Help: "Total outbox records permanently failed after max retries — requires human intervention",
	},
)

// OutboxPendingGauge 当前待处理的 REDIS_DONE outbox 记录数（积压量）
// OutboxWorker 每次 processBatch 后更新此值
var OutboxPendingGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "accounting_outbox_pending_records",
		Help: "Current number of REDIS_DONE outbox records waiting for MySQL write (lag indicator)",
	},
)

// OutboxOldestPendingAgeSeconds 当前积压队列里最老的 outbox 记录的年龄（秒）。
// 比 pending_records 更直接反映 lag：积压 1 万但都是 1s 内的没事；积压 10 但
// 最老的 5 分钟没动 = MySQL 写入卡住，告警。
//
// 推荐 SLO：P99 < 30s；P0 告警阈值 > 5min。
var OutboxOldestPendingAgeSeconds = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "accounting_outbox_oldest_pending_age_seconds",
		Help: "Age (seconds) of the oldest REDIS_DONE outbox record waiting for MySQL write",
	},
)

// OutboxMySQLWriteDuration outbox 单条持久化到 MySQL 的耗时直方图
var OutboxMySQLWriteDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "accounting_outbox_mysql_write_duration_seconds",
		Help:    "Duration of a single outbox record MySQL write (account_transaction + balance update)",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
	},
)

// ─── TCC 阶段耗时直方图 ───────────────────────────────────────────────────────
//
// 三个阶段独立观测，便于定位"具体哪一段慢"：Try 一般是冻结资金锁竞争，
// Confirm 是写流水 + 余额更新（最重），Cancel 解冻几乎是 inverse Try。
// 同一 buckets 让 Grafana 直接横向比较三段 p99。

var tccPhaseBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5}

// TccTryDuration 单条 TCC 分支 Try 阶段耗时（含获取行锁 + UPDATE available_balance）。
var TccTryDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "accounting_tcc_try_duration_seconds",
		Help:    "Duration of a single TCC branch Try phase",
		Buckets: tccPhaseBuckets,
	},
)

// TccConfirmDuration 单条 TCC 分支 Confirm 阶段耗时（INSERT account_transaction + UPDATE balance）。
var TccConfirmDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "accounting_tcc_confirm_duration_seconds",
		Help:    "Duration of a single TCC branch Confirm phase (transaction insert + balance update)",
		Buckets: tccPhaseBuckets,
	},
)

// TccCancelDuration 单条 TCC 分支 Cancel 阶段耗时（解冻 + 状态置 CANCELLED）。
var TccCancelDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "accounting_tcc_cancel_duration_seconds",
		Help:    "Duration of a single TCC branch Cancel phase",
		Buckets: tccPhaseBuckets,
	},
)

// ─── Recovery ─────────────────────────────────────────────────────────────────

// RecoveryTotal 恢复成功计数器（区分 type）
// type: "outbox_pending" | "order_processing"
var RecoveryTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "accounting_recovery_total",
		Help: "Total records successfully recovered from stuck state",
	},
	[]string{"type"},
)

// RecoveryFailuresTotal 恢复失败计数器（达到重试上限，需人工介入）
// type: "outbox_pending" | "order_processing"
var RecoveryFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "accounting_recovery_failures_total",
		Help: "Total recovery attempts that permanently failed after max retries — requires human intervention",
	},
	[]string{"type"},
)

// ─── TCC Recovery ─────────────────────────────────────────────────────────────

// TccRecoveredTotal TCC 自动回滚的分支/事务数（服务崩溃后 TRYING 超时自动取消）
var TccRecoveredTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_tcc_recovered_total",
		Help: "Total TCC transactions automatically cancelled after stuck TRYING timeout",
	},
)

// TccRecoveryFailuresTotal TCC 自动回滚失败次数（分支取消失败，冻结资金未释放，需人工介入）
var TccRecoveryFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_tcc_recovery_failures_total",
		Help: "Total TCC recovery failures — frozen balance NOT released, requires human intervention",
	},
)

// ─── TCC 协调者状态机 ─────────────────────────────────────────────────────────

// TccCoordinatorTransitionsTotal TCC 协调者状态机转换计数（按 from→to 维度）。
// 异常标签（例如 rejected_illegal）指示竞态命中了 WHERE phase=expected 保护层，
// 非零通常表示 Recovery/Confirm 竞态，需要关注。
var TccCoordinatorTransitionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "accounting_tcc_coordinator_transitions_total",
		Help: "TCC coordinator phase-transition attempts labeled by from/to/outcome (ok|rejected_illegal|error)",
	},
	[]string{"from", "to", "outcome"},
)

// ─── Watchdog / 日切 ──────────────────────────────────────────────────────────

// WatchdogStuckShardsGauge 当前 WatchdogRecover tick 检测到的 stuck 分片数。
// 非零且持续增长通常意味着日切主流程出问题（该值 > 0 应告警）。
var WatchdogStuckShardsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "accounting_watchdog_stuck_shards",
		Help: "Number of day-cut shards detected as stuck in the most recent watchdog tick",
	},
)

// WatchdogRecoveriesTriggeredTotal watchdog 实际触发（进入 semaphore）的分片恢复数。
var WatchdogRecoveriesTriggeredTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_watchdog_recoveries_triggered_total",
		Help: "Total shard recoveries triggered by day-cut watchdog",
	},
)

// ─── Redis 热路径幂等 ────────────────────────────────────────────────────────

// TransferIdempotencyHitsTotal Redis Transfer 的 voucher 级幂等哨兵命中次数。
// 非零通常来自 OutboxWorker 对 PENDING 重试 —— 命中说明幂等保护确实阻止了 double-count。
var TransferIdempotencyHitsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_transfer_idempotency_hits_total",
		Help: "Number of Redis Transfer calls rejected by voucher-level idempotency sentinel (retry safe)",
	},
)

// ─── TCC 归档 ────────────────────────────────────────────────────────────────

// TccArchivedTotal 从 tcc_transaction 中归档/删除的终态分支数（CONFIRMED / CANCELLED 且超过保留期）。
var TccArchivedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_tcc_archived_total",
		Help: "Total TCC branch records deleted from tcc_transaction as part of retention cleanup",
	},
)

// TccArchiveErrorsTotal TCC 归档失败次数（DB 错误等）。
var TccArchiveErrorsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_tcc_archive_errors_total",
		Help: "TCC archive worker shard errors (non-fatal, retries on next tick)",
	},
)

// DeadlockRetryExhaustedTotal 死锁/瞬时可重试错误重试 N 次仍失败的计数。
// 该值非零说明某热点账户竞争过重，需要考虑拆分或降并发。Prometheus 告警
// (rules.yml: MySQLRetryExhausted) 观察此指标。
var DeadlockRetryExhaustedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "accounting_deadlock_retry_exhausted_total",
		Help: "Transient MySQL errors (deadlock/lock-wait) that exhausted retryOnDeadlock budget",
	},
)

// InflightBookingsGauge 当前正在处理中的 booking 数。
// Graceful shutdown 时 readiness probe 先转 503 摘流量，然后等这个 gauge 归零
// （或 drain 超时）再关进程，避免 K8s rollout 打断 TCC Confirm 导致 stuck TRYING。
var InflightBookingsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "accounting_inflight_bookings",
		Help: "Number of bookings currently in flight (entered DoubleEntryBooking, not yet returned)",
	},
)

// LoadShedDroppedTotal 限流 / 入口饱和时被 fast-fail 拒绝的请求数。
// 非零 + 持续 → 系统过载，需要扩容或调整限流阈值。
var LoadShedDroppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "accounting_load_shed_dropped_total",
		Help: "Requests rejected by load shedding interceptor",
	},
	[]string{"reason"}, // reason: "max_inflight" | "rate_limit"
)

// ─── DB 连接池可观测性 ─────────────────────────────────────────────────────────
//
// 由 cmd/server 启动后台 ticker（每 10s）调 Manager.GetAllDBs() + GetMetaDB()
// 抓取 *sql.DB.Stats() 写入这些 gauge。Grafana 推荐看板：
//   - in_use / max_open_conns 利用率（>80% 持续 → 扩 pool 或加副本）
//   - rate(wait_count[5m]) 与 wait_seconds 的差分（饱和期内单次平均等待）
//   - 各 shard 利用率对比，识别热点分片

var DBPoolOpenConns = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "accounting_db_pool_open_conns",
		Help: "Number of established connections (in_use + idle) per shard",
	},
	[]string{"shard"}, // "meta" | "0".."N-1"
)

var DBPoolInUseConns = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "accounting_db_pool_in_use_conns",
		Help: "Number of connections currently in use per shard",
	},
	[]string{"shard"},
)

var DBPoolIdleConns = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "accounting_db_pool_idle_conns",
		Help: "Number of idle connections per shard",
	},
	[]string{"shard"},
)

// 累计型 gauge（值是 sql.DBStats 的 WaitCount，本身就是 monotonic counter）
var DBPoolWaitCount = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "accounting_db_pool_wait_count",
		Help: "Cumulative number of connections waited for per shard (sql.DBStats.WaitCount)",
	},
	[]string{"shard"},
)

var DBPoolWaitSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "accounting_db_pool_wait_seconds",
		Help: "Cumulative seconds blocked waiting for a connection per shard (sql.DBStats.WaitDuration)",
	},
	[]string{"shard"},
)

var DBPoolMaxOpenConns = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "accounting_db_pool_max_open_conns",
		Help: "Configured max open connections per shard (from SetMaxOpenConns)",
	},
	[]string{"shard"},
)

// BalanceCacheKeyBytes 单条 balance: cache key 大小观察直方图。
// **P2-7 大 key 监控**：shadow 流量 + 高频缓冲账户的 pending 字段累积，单条
// HSET balance:<accountNo>_shadow 的 value 可能膨胀到 MB 级别，超过 Redis 单 key
// 写入 buffer 上限会让客户端报 "ERR Protocol error: invalid bulk length"。
// alerting：直方图 +Inf bucket 累积速率 > 1/min 立即拉警，运维 redis-cli MEMORY
// USAGE <key> 看具体哪条爆掉，定位是哪个账户。
//
// label dim：main / shadow（互相隔开 shadow 压测污染主流量观测）
var BalanceCacheKeyBytes = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "accounting_balance_cache_key_bytes",
		Help:    "Size in bytes of a single Redis balance cache key on write (alerts > 10MB indicate runaway buffering).",
		Buckets: []float64{1024, 8192, 65536, 262144, 1048576, 10485760, 104857600},
	},
	[]string{"dim"},
)

// ─── 注册 & HTTP Server ───────────────────────────────────────────────────────

// Register 注册所有 metrics 到默认 Prometheus registry
func Register() {
	prometheus.MustRegister(
		BookingTotal,
		BalanceCacheKeyBytes,
		WarmAccountsTotal,
		HotPathFallbackTotal,
		OutboxProcessedTotal,
		OutboxFailedTotal,
		OutboxPendingGauge,
		OutboxOldestPendingAgeSeconds,
		OutboxMySQLWriteDuration,
		RecoveryTotal,
		RecoveryFailuresTotal,
		TccRecoveredTotal,
		TccRecoveryFailuresTotal,
		TccCoordinatorTransitionsTotal,
		WatchdogStuckShardsGauge,
		WatchdogRecoveriesTriggeredTotal,
		TransferIdempotencyHitsTotal,
		TccArchivedTotal,
		TccArchiveErrorsTotal,
		DeadlockRetryExhaustedTotal,
		InflightBookingsGauge,
		LoadShedDroppedTotal,
		DBPoolOpenConns,
		DBPoolInUseConns,
		DBPoolIdleConns,
		DBPoolWaitCount,
		DBPoolWaitSeconds,
		DBPoolMaxOpenConns,
		TccTryDuration,
		TccConfirmDuration,
		TccCancelDuration,
		WarmAccountFailuresTotal,
	)
}

// RegisterDBStats 注册 DB 连接池指标到默认 registry。dbs 一般来自
// database.Manager.SQLDBs()，key 是 shard 标签（"shard0", "meta" 等）。
//
// 暴露指标见 healthx.DBStatsCollector：accounting_db_open_connections /
// in_use / idle / max_open / wait_count_total / wait_seconds_total，
// 都带 db label。已注册过则忽略 panic（fx 重启场景 / 测试）。
func RegisterDBStats(dbs map[string]*sql.DB) {
	c := healthx.NewMultiDBStatsCollector("accounting", dbs)
	if err := prometheus.Register(c); err != nil {
		// AlreadyRegisteredError 可忽略；其它错误 panic
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}

// StartServer 在指定端口启动 Prometheus /metrics HTTP 接口（非阻塞，在 goroutine 中运行）。
// 显式 timeout 防止 slowloris：默认 http.ListenAndServe 走 DefaultServeMux 无任何 timeout，
// 是公认的 CWE-400 漏洞模式。
func StartServer(port string, logger *zap.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		logger.Info("prometheus metrics server started", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil {
			logger.Error("prometheus metrics server stopped", zap.Error(err))
		}
	}()
}
