// commercial.go: 商业化必备的 SLO / 业务监控指标 + admin auth 中间件。
//
// 默认 metrics.go 里的指标只够调试；商业部署还需要：
//   - 按商户拆分的 verdict 率 → 计费 + 计算 fraud rate / approval rate SLA
//   - score 分布 → 看模型 / 规则是否在合理区间，避免大批量在阈值附近震荡
//   - review queue depth → 运营值班负载，是否需要扩容
//   - outcome rate → 评估 ML 模型 precision / recall 的实时反馈
package metrics

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiongwp/risk-manage/internal/auth"
)

// keyHashPrefix sha256(token) 前 16 字符做稳定 KeyID（log audit 用）。
// 不能直接落原 token；hash 防 token 通过审计反推。
func keyHashPrefix(token string) string {
	h := sha256.Sum256([]byte(token))
	return "tok_" + hex.EncodeToString(h[:8])
}

// _ context import 避免空 import
var _ = context.Background

// VerdictTotal 按商户 × verdict 计数。商业关键指标：每商户的 approval /
// review / deny 比例直接跟入金率挂钩，运营按周对账。
var VerdictTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_verdict_total",
	Help: "Final verdict counts per merchant",
}, []string{"merchant_id", "verdict"})

// RiskScore 风险分分布。看大盘是否在阈值附近聚集（说明阈值需要调）。
var RiskScore = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "risk_score",
	Help:    "Risk score distribution (per Screen call)",
	Buckets: []float64{0, 5, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100},
}, []string{"merchant_id"})

// ReviewQueueDepth pending review 数。值班 SRE 的关键工作面板。
var ReviewQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_review_queue_depth",
	Help: "Number of pending review items waiting for human decision",
})

// ReviewQueueOldestPendingAgeSeconds 当前 pending 队列里最早一条的年龄（秒）。
//
// 跟 ReviewQueueDepth 互补：
//   - depth=10 但 oldest=2min  -> 正常吞吐
//   - depth=10 但 oldest=2h    -> 处理停摆 / SLA 已破
//
// 由 cmd/server 后台 goroutine 周期采样（与 ReviewQueueDepth 同 tick）。
// 没有 pending 时设为 0。
var ReviewQueueOldestPendingAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_review_queue_oldest_pending_age_seconds",
	Help: "Age (seconds) of the oldest pending review item; 0 if empty",
})

// OutcomeTotal 反馈来源 × is_fraud 计数。算 ML precision/recall 的输入。
var OutcomeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "risk_outcome_total",
	Help: "Outcome feedback by source and fraud label",
}, []string{"source", "is_fraud"})

// OutcomeLagSeconds 决策时间到 outcome 反馈到达的延迟分布。按 source 拆分：
//   - review_human：小时级（人工审核 SLA 内）
//   - dispute：天 ~ 月级（chargeback 流程慢）
//   - merchant_confirm：分 ~ 小时级
//
// 用于 alerting：dispute lag > 60d 的 P95 说明 chargeback 上游有积压；
// review_human lag > SLA 多说明值班不足。
var OutcomeLagSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "risk_outcome_lag_seconds",
	Help: "Time from decision to outcome feedback arrival, by source",
	Buckets: []float64{
		60, 600, 3600,                          // 1m / 10m / 1h
		3 * 3600, 12 * 3600, 24 * 3600,         // 3h / 12h / 1d
		3 * 86400, 7 * 86400, 30 * 86400,       // 3d / 7d / 30d
		60 * 86400, 90 * 86400, 180 * 86400,    // 60d / 90d / 180d
	},
}, []string{"source"})

// OutcomeCoverageRatio 最近 N 天决策中至少拿到一条 outcome 反馈的比例。
// 由 cmd/server 的后台 goroutine 周期采样（默认 5 分钟）写入。
//
// 给 ML pipeline 看健康度：coverage < 30% 说明反馈链路坏了，
// 这时跑 retrain 出来的模型 = bias 噪声。值班 SRE 看 alert：
//   ALERT: risk_outcome_coverage_ratio{verdict="DENY"} < 0.5 持续 1h
// = order-core dispute hook 挂了 / merchant_confirm 端点 down。
var OutcomeCoverageRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_outcome_coverage_ratio",
	Help: "Fraction of recent decisions with at least one outcome record (sampled, by verdict)",
}, []string{"verdict"})

// OutcomeCoverageWindow gauge 跑覆盖率统计的时间窗口大小（秒）。给 Grafana
// 在面板上显示 "最近 X 天 coverage = Y%" 的 X 用。
var OutcomeCoverageWindow = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_outcome_coverage_window_seconds",
	Help: "Time window over which OutcomeCoverageRatio is computed",
})

// RuleLastHitAge 每条规则距最近一次命中的秒数。> 7d 默认值 = 规则可能已经
// silent（被绕过 / 配置过时 / 数据格式变了）。值为 -1 = 从未命中（在窗口内）。
//
// 对应告警：
//
//	risk_rule_last_hit_age_seconds > 7 * 24 * 3600 持续 1h
//	→ 通知值班 SRE 或规则 owner，让他们检查规则是否还有效
//
// 由 cmd/server 后台 worker 用 ruleinsights.Compute 周期采样写入。
var RuleLastHitAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_rule_last_hit_age_seconds",
	Help: "Seconds since each rule's last hit; -1 if never hit in window",
}, []string{"rule_id"})

// RulePrecision / RuleROI 给运营 dashboard 看每条规则的"准确度"和"价值"。
// 见 ruleinsights.RuleStats 详细计算。
var RulePrecision = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_rule_precision",
	Help: "Per-rule precision in window (fraud / labeled hits); 0 means insufficient labels",
}, []string{"rule_id"})

var RuleROI = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_rule_roi",
	Help: "Per-rule ROI = hits × precision; high = both fires often AND is right",
}, []string{"rule_id"})

// CounterCacheHit / CounterCacheMiss CachedCounter 命中率指标。
// 命中率 < 60% 持续 = TTL 太短 / cache size 不够 / 流量分布太散；
// 调 counter.cache_ttl 加大 ttl，或加大 cache。
var CounterCacheHit = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "risk_counter_cache_hit_total",
	Help: "CachedCounter hits (saves a Redis/DB round-trip)",
})
var CounterCacheMiss = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "risk_counter_cache_miss_total",
	Help: "CachedCounter misses (forwarded to inner Counter)",
})
var CounterCacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_counter_cache_size",
	Help: "CachedCounter current entry count (cap=4096; close to cap → consider tuning)",
})

// WebhookDLQDepth 死信队列深度。给运营 dashboard 看 webhook 推送可靠性。
// > 0 持续 = 商户后端持续失败 / risk-manage 配置错；admin 可查 DLQ 详情
// 决定 replay / discard。
var WebhookDLQDepth = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_webhook_dlq_depth",
	Help: "Number of events in webhook DLQ (retries exhausted)",
})

// MLScoreMean / MLScoreP95 / MLScoreDriftPct ML 分数分布漂移监控的 Prometheus
// 面板。Grafana 看 ML 输出趋势 + 触发 alert（threshold drifted）。
//
// 由 main.go 起一个 60s ticker 从 mlscore.DriftMonitor.Snapshot() / IsDrifted()
// 拉数据更新。
var MLScoreMean = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_ml_score_mean",
	Help: "Mean ML risk score over the recent reservoir",
})

var MLScoreP95 = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_ml_score_p95",
	Help: "P95 ML risk score over the recent reservoir",
})

var MLScoreDrifted = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_ml_score_drifted",
	Help: "1 = ML score distribution drifted from baseline (any metric > threshold), 0 = stable",
})

var MLScoreDriftMeanPct = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "risk_ml_score_drift_mean_pct",
	Help: "Percentage delta of current mean vs baseline mean (signed)",
})

// RiskDriftPSI per-feature PSI gauge。severity label：ok / warning / critical /
// no_baseline / insufficient。同一特征只会有一条 severity 上是非零值（其它清零），
// 用 max() 在 PromQL 侧聚合即可。
var RiskDriftPSI = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_drift_psi",
	Help: "Per-feature Population Stability Index (current vs baseline)",
}, []string{"feature", "severity"})

// RiskDriftKS per-feature KS 统计量（D）。pvalue 单独一条 gauge 方便 alert
// 直接 alert on (p < 0.01 AND D > 0.1)。
var RiskDriftKS = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_drift_ks_statistic",
	Help: "Per-feature KS two-sample statistic D (max CDF distance)",
}, []string{"feature"})

var RiskDriftKSPValue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "risk_drift_ks_pvalue",
	Help: "Per-feature KS p-value (Kolmogorov asymptotic). Low p = different distribution.",
}, []string{"feature"})

// RegisterCommercial 在 Register() 之外注册商业化指标。Register() 是 sync.Once，
// 这里独立 init 让没接入商业 sink 的部署也能跑（虽然指标只是 Collector 注册）。
func init() {
	prometheus.MustRegister(VerdictTotal, RiskScore, ReviewQueueDepth,
		ReviewQueueOldestPendingAgeSeconds, OutcomeTotal,
		OutcomeLagSeconds, OutcomeCoverageRatio, OutcomeCoverageWindow,
		RuleLastHitAge, RulePrecision, RuleROI,
		CounterCacheHit, CounterCacheMiss, CounterCacheSize,
		WebhookDLQDepth,
		MLScoreMean, MLScoreP95, MLScoreDrifted, MLScoreDriftMeanPct,
		RiskDriftPSI, RiskDriftKS, RiskDriftKSPValue)
}

// AdminAuth bearer-token 中间件。tokens 为空 → 完全放行（dev 模式）。
// 推荐做法：
//   - 生产 tokens 里只放 admin-web / dispute 系统的服务账号 token
//   - api-gateway 上层再加一层 mTLS / IP allowlist 双保险
//   - 真正人工操作走 admin-web，由其转发到此服务（带服务账号 token + actor 字段）
//
// header: Authorization: Bearer <token>
//
// 兼容旧 API：旧调用方传 map[string]struct{}。新 RBAC 调用方应该用
// AdminAuthRoles 传 map[token]role。本函数等价 token → role="danger"。
func AdminAuth(tokens map[string]struct{}) func(http.Handler) http.Handler {
	roles := make(map[string]string, len(tokens))
	for k := range tokens {
		roles[k] = "danger"
	}
	return AdminAuthRoles(roles)
}

// AdminAuthRoles RBAC bearer-token 中间件。每个 token 映射到一个 role
// (read / write / danger)。验证通过后把 *auth.Principal{KeyID: keyID,
// AdminRole: role} 注入 ctx，下游 handler 用 RequireRole 判权。
//
// roles 为空 → 完全放行（dev 等价老行为）。
func AdminAuthRoles(roles map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(roles) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			const p = "Bearer "
			if len(h) <= len(p) || h[:len(p)] != p {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			tok := h[len(p):]
			var matchedRole, matchedToken string
			for t, role := range roles {
				if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
					matchedRole = role
					matchedToken = t
					break
				}
			}
			if matchedRole == "" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			// 把 admin Principal 注入 ctx，下游 RequireRole / handler 可读
			p2 := &auth.Principal{
				KeyID:     keyHashPrefix(matchedToken),
				AdminRole: matchedRole,
			}
			ctx := auth.WithPrincipal(r.Context(), p2)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole handler middleware：要求 ctx Principal 有指定 role 才放行。
// 用法：
//
//	mux.Handle("/admin/rules/update", RequireRole("danger")(updateHandler))
//	mux.Handle("/admin/rules/list",   RequireRole("read")(listHandler))
//
// 请求未经过 AdminAuthRoles → ctx 无 Principal → 视作 dev 模式放行（兼容
// 老 AdminAuth 调用方）。
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if !ok || p == nil {
				// 未鉴权 (dev) → 放行；生产应该确保 AdminAuthRoles 中间件先跑
				next.ServeHTTP(w, r)
				return
			}
			if !p.HasRole(role) {
				http.Error(w, `{"error":"forbidden: role insufficient (need `+role+`)"}`,
					http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AdminMux 给 admin/* 端点用的 mux 包装：
//
//	parent.Handle(prefix, AdminMux(adminMux, AdminAuth(tokens)))
//
// 让 admin 路由集中带 auth；公网路径（/v1/risk/session、/healthz、/metrics）
// 独立挂在 parent 上，不受影响。
func AdminMux(adminMux *http.ServeMux, auth func(http.Handler) http.Handler) http.Handler {
	if auth == nil {
		return adminMux
	}
	return auth(adminMux)
}
