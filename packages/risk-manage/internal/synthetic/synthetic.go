// Package synthetic —— 合成监控探针（synthetic monitoring）。
//
// 商业级风控 SOP：定期发"已知应该 X verdict"的固定 fixture txn 给 Screen，
// 验证 actual==expected。一旦 mismatch → Prometheus alert → on-call。
//
// 这种 silent-drop 类故障历史上踩过：
//   - rules-recipes.yaml 漏拷贝进镜像，rule_count=12 而非 60
//   - dsl 规则 caller metadata 字段拼错（拼错了 → 永远不 match → 永远 ALLOW）
//   - 阈值配错（threshold=0 / max_count=0 → 规则永远不 hit）
//
// 普通业务流量看不出来这些 bug（只是攻击者多通过了几个，风控 ALLOW 比例
// 缓慢上升不会立刻 alert）；synthetic 用绝对断言"这个 fixture 必须 DENY"
// 让 silent regression 在 1 分钟内被抓到。
//
// 实现：
//   1. 固定 fixture 集合（已知场景 → 期望 verdict）
//   2. 每 N 秒跑一遍，调 Screen
//   3. metrics.SyntheticProbeTotal{name, result=ok|mismatch|error}
//   4. mismatch 时 log warning + 详细 hits dump（debugging 友好）
package synthetic

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/metrics"
)

// Probe 一个固定测试用例。
type Probe struct {
	Name           string         // 唯一名（metric label / log）
	Description    string         // 给 dashboard 展示用
	Build          func() *engine.TxnContext // 每次重新构造避免脏状态
	ExpectedVerdict engine.Decision           // engine.Allow / Review / Deny
	// ExpectedRuleHits 至少应命中的规则 ID（不要求 exact，subset OK）
	// 让运维加新规则时不会立刻让 synthetic 翻红。
	ExpectedRuleHits []string
}

// Screener 抽象化 svc.Screen 接口，避免循环 import（service 包引用本包就能跑）。
type Screener interface {
	Screen(ctx context.Context, txn *engine.TxnContext) *engine.Result
}

// Worker 后台跑探针的 fx-managed loop。
type Worker struct {
	probes   []Probe
	screener Screener
	interval time.Duration
	logger   *zap.Logger
}

func NewWorker(screener Screener, probes []Probe, interval time.Duration, logger *zap.Logger) *Worker {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Worker{probes: probes, screener: screener, interval: interval, logger: logger}
}

// Start 阻塞跑直到 ctx done。main.go fx hook OnStart 用 go w.Start(ctx)。
func (w *Worker) Start(ctx context.Context) {
	if len(w.probes) == 0 || w.screener == nil {
		return
	}
	// 启动 5s 后跑首轮，避免跟其它启动期初始化竞争 CPU
	first := time.NewTimer(5 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	w.runAll(ctx)
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.runAll(ctx)
		}
	}
}

func (w *Worker) runAll(parent context.Context) {
	for _, p := range w.probes {
		w.runOne(parent, p)
	}
}

func (w *Worker) runOne(parent context.Context, p Probe) {
	if p.Build == nil {
		metrics.SyntheticProbeTotal.WithLabelValues(p.Name, "error").Inc()
		w.logger.Warn("synthetic probe missing Build", zap.String("name", p.Name))
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	txn := p.Build()
	if txn == nil {
		metrics.SyntheticProbeTotal.WithLabelValues(p.Name, "error").Inc()
		return
	}
	res := w.screener.Screen(ctx, txn)
	if res == nil {
		metrics.SyntheticProbeTotal.WithLabelValues(p.Name, "error").Inc()
		w.logger.Warn("synthetic probe nil result", zap.String("name", p.Name))
		return
	}
	if res.Decision != p.ExpectedVerdict {
		metrics.SyntheticProbeTotal.WithLabelValues(p.Name, "mismatch").Inc()
		w.logger.Warn("synthetic probe verdict mismatch",
			zap.String("name", p.Name),
			zap.String("expected", p.ExpectedVerdict.String()),
			zap.String("actual", res.Decision.String()),
			zap.Int("score", res.RiskScore),
			zap.Strings("hits", hitIDs(res)))
		return
	}
	if !subsetHit(p.ExpectedRuleHits, res) {
		metrics.SyntheticProbeTotal.WithLabelValues(p.Name, "mismatch").Inc()
		w.logger.Warn("synthetic probe expected rule did not hit",
			zap.String("name", p.Name),
			zap.Strings("expected_rules", p.ExpectedRuleHits),
			zap.Strings("hits", hitIDs(res)))
		return
	}
	metrics.SyntheticProbeTotal.WithLabelValues(p.Name, "ok").Inc()
}

func subsetHit(want []string, res *engine.Result) bool {
	if len(want) == 0 {
		return true
	}
	got := map[string]struct{}{}
	for _, h := range res.Hits {
		got[h.RuleID] = struct{}{}
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			return false
		}
	}
	return true
}

func hitIDs(res *engine.Result) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.RuleID)
	}
	return out
}

// DefaultProbes 出厂自带的核心 fixture 集合。各项断言：
//
//   1. clean_signup_allow：完全干净的 signup → 必须 ALLOW
//   2. disposable_email_register_deny：mailinator → disposable_email_register hit + DENY
//   3. high_amount_review：超大额支付 → 至少触发 amount_limit 类规则
//
// 不依赖外部状态（LinkStore / Counter 历史），每次都从零构造 → 重启即过。
func DefaultProbes() []Probe {
	return []Probe{
		{
			Name:        "clean_signup_allow",
			Description: "干净的 signup（无任何风险信号）应该 ALLOW",
			Build: func() *engine.TxnContext {
				return &engine.TxnContext{
					EventType:  "register",
					MerchantID: "synthetic",
					IPAddress:  "203.0.113.10", // RFC 5737 documentation range
					DeviceID:   "synthetic-clean",
					UserAgent:  "Mozilla/5.0 SyntheticProbe",
					// Platform=ios 让 client_tampering 规则跳过 direct_api_call 信号
					// （移动端 API 调用本来就没 RiskSessionID，不算"绕过 SDK"）
					Platform: "ios",
					// RiskSessionID + DeviceFingerprintHash 模拟 SDK 上送过信号，
					// 避免 client_tampering 的 direct_api_call(+2 score) 触发。
					RiskSessionID:         "synth_sess_clean",
					DeviceFingerprintHash: "fp_synth_clean_abc123",
					SignalCoverageRatio:   0.9,
					Metadata: map[string]string{
						"email_domain":     "gmail.com",
						"account_age_days": "30",
						// full_name 喂给 sanction_check：required=true 时缺即 REVIEW；
						// 合成探针提供假名走 OK 路径，避免"未筛查"挡住 ALLOW。
						"full_name": "Synthetic TestUser",
					},
				}
			},
			ExpectedVerdict: engine.Allow,
		},
		{
			Name:        "disposable_email_register_deny",
			Description: "mailinator.com 注册必须 deny（disposable_email 规则；规则实际 ID 是 disposable_email 非 _register）",
			Build: func() *engine.TxnContext {
				return &engine.TxnContext{
					EventType:  "register",
					MerchantID: "synthetic",
					IPAddress:  "203.0.113.11",
					DeviceID:   "synthetic-disp",
					UserAgent:  "Mozilla/5.0 SyntheticProbe",
					Metadata: map[string]string{
						"email_domain":     "mailinator.com",
						"account_age_days": "0",
					},
				}
			},
			ExpectedVerdict:  engine.Deny,
			// 改正 ExpectedRuleHits：旧值 "disposable_email_register" 是错的；
			// 实际规则注册名是 "disposable_email"（cmd/server/main.go:801）。
			ExpectedRuleHits: []string{"disposable_email"},
		},
		{
			Name:        "high_risk_country_review",
			Description: "高风险国家 + 大额 → 至少 REVIEW",
			Build: func() *engine.TxnContext {
				return &engine.TxnContext{
					EventType:  "payment",
					MerchantID: "synthetic",
					Country:    "NG",
					IPCountry:  "NG",
					Amount:     50_000_00, // ¥50,000 (in cents-equivalent, will trigger limit)
					Currency:   "PHP",
					IPAddress:  "203.0.113.12",
					DeviceID:   "synthetic-high",
					Metadata: map[string]string{
						"account_age_days": "30",
					},
				}
			},
			ExpectedVerdict: engine.Review,
		},
	}
}

// HitsToString debug-friendly 把 hits 拼成易读的 csv（给 alert payload 用）。
func HitsToString(res *engine.Result) string {
	if res == nil {
		return ""
	}
	return strings.Join(hitIDs(res), ",")
}
