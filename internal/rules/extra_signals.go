// extra_signals.go: 4 条 P1 商业规则。
//
//   - impossible_travel：同 customer / device 跨洲际短时间出现（geo distance / Δt > 1000 km/h）
//   - returning_customer：成功支付历史多 + 无 chargeback → 信任分（命中即 Force-Allow，
//     绕过中等风险规则）
//   - avs_check：billing 地址 vs 卡 issuer 地址不一致
//   - bin_country：卡 BIN 国家 vs 交易声明国家不一致
//
// 这些规则的输入数据由 payment-core 在调 Screen 前准备好，写到 metadata：
//   metadata.bin_country, metadata.avs_response, metadata.returning_customer_paid_count, ...
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── impossible_travel ───────────────────────────────────────────
//
// 假设 LinkStore 里有 customer:X → ip:Y 边的 last-seen 时间戳信息（当前接口
// 没暴露）。简化版：用 metadata 预先算好的字段：
//
//	metadata.last_ip_country  上次成功支付的 IP 国家
//	metadata.last_seen_min    距上次成功支付的分钟数
//	metadata.geo_distance_km  本次 IP 跟上次 IP 的地理距离 (km)
//
// 规则：last_seen_min 在窗口内（< window_min）且 distance / hours > speed_kmh
// → impossible_travel 命中。

type ImpossibleTravelConfig struct {
	WindowMin int    `json:"window_min"` // 仅在窗口内启用；默认 360 (6h)
	SpeedKmh  int    `json:"speed_kmh"`  // 速率上限；默认 1000 (民航极限)
	Decision  string `json:"decision"`
	Weight    int    `json:"weight"`
}

type impossibleTravelRule struct {
	id, name string
	enabled  bool
	cfg      ImpossibleTravelConfig
	verdict  engine.Decision
}

func ImpossibleTravelFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg ImpossibleTravelConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("impossible_travel: %w", err)
			}
		}
		if cfg.WindowMin <= 0 {
			cfg.WindowMin = 360
		}
		if cfg.SpeedKmh <= 0 {
			cfg.SpeedKmh = 1000
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &impossibleTravelRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v}, nil
	}
}

func (r *impossibleTravelRule) ID() string    { return r.id }
func (r *impossibleTravelRule) Name() string  { return r.name }
func (r *impossibleTravelRule) Type() string  { return "impossible_travel" }
func (r *impossibleTravelRule) Enabled() bool { return r.enabled }
func (r *impossibleTravelRule) Weight() int   { return r.cfg.Weight }

func (r *impossibleTravelRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	lastSeen, err := strconv.Atoi(txn.Metadata["last_seen_min"])
	if err != nil || lastSeen <= 0 || lastSeen > r.cfg.WindowMin {
		return nil
	}
	dist, err := strconv.ParseFloat(txn.Metadata["geo_distance_km"], 64)
	if err != nil || dist <= 0 {
		return nil
	}
	hours := float64(lastSeen) / 60.0
	if hours <= 0 {
		return nil
	}
	speed := dist / hours
	if int(speed) <= r.cfg.SpeedKmh {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("impossible travel: %.0fkm in %dmin = %.0fkm/h (limit %d)",
			dist, lastSeen, speed, r.cfg.SpeedKmh),
	}
}

// ─── returning_customer ──────────────────────────────────────────
//
// 老客户信任分：商户 metadata 里写过去 90 天的成功 paid 笔数 + chargeback 笔数。
// 多笔成功 + 零 chargeback → Force-Allow（绕过中等风险规则）；
// 任何 chargeback 历史 → 不命中（不打分但不阻断）。

type ReturningCustomerConfig struct {
	MinPaidCount int  `json:"min_paid_count"`         // 命中阈值；默认 5
	MaxChargeback int `json:"max_chargeback"`         // 允许多少历史 chargeback（一般 0）；默认 0
	ForceAllow    bool `json:"force_allow"`           // true = Force=true 短路；默认 true
}

type returningCustomerRule struct {
	id, name string
	enabled  bool
	cfg      ReturningCustomerConfig
}

func ReturningCustomerFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg ReturningCustomerConfig
		cfg.ForceAllow = true
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("returning_customer: %w", err)
			}
		}
		if cfg.MinPaidCount <= 0 {
			cfg.MinPaidCount = 5
		}
		return &returningCustomerRule{id: id, name: name, enabled: enabled, cfg: cfg}, nil
	}
}

func (r *returningCustomerRule) ID() string    { return r.id }
func (r *returningCustomerRule) Name() string  { return r.name }
func (r *returningCustomerRule) Type() string  { return "returning_customer" }
func (r *returningCustomerRule) Enabled() bool { return r.enabled }

func (r *returningCustomerRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	paid, _ := strconv.Atoi(txn.Metadata["customer_paid_count_90d"])
	if paid < r.cfg.MinPaidCount {
		return nil
	}
	cb, _ := strconv.Atoi(txn.Metadata["customer_chargeback_count_90d"])
	if cb > r.cfg.MaxChargeback {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: engine.Allow,
		Force:    r.cfg.ForceAllow,
		Detail: fmt.Sprintf("trusted returning customer: %d paid / %d chargebacks in 90d", paid, cb),
	}
}

// ─── avs_check ───────────────────────────────────────────────────
//
// 标准 AVS 响应码（payment-core 从渠道拿到后塞 metadata.avs_response）：
//   Y / X = 完全匹配；A = 仅地址；Z = 仅 zip；N = 全不匹配；U = 不可用
// N → Force-Deny（明确不匹配）；A / Z → review（部分匹配，加分）。

type AVSConfig struct {
	StrictDecision string `json:"strict_decision"` // N 命中时的 verdict; 默认 deny
	PartialDecision string `json:"partial_decision"` // A / Z; 默认 review
}

type avsCheckRule struct {
	id, name string
	enabled  bool
	cfg      AVSConfig
}

func AVSCheckFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg AVSConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("avs_check: %w", err)
			}
		}
		if cfg.StrictDecision == "" {
			cfg.StrictDecision = "deny"
		}
		if cfg.PartialDecision == "" {
			cfg.PartialDecision = "review"
		}
		return &avsCheckRule{id: id, name: name, enabled: enabled, cfg: cfg}, nil
	}
}

func (r *avsCheckRule) ID() string    { return r.id }
func (r *avsCheckRule) Name() string  { return r.name }
func (r *avsCheckRule) Type() string  { return "avs_check" }
func (r *avsCheckRule) Enabled() bool { return r.enabled }

func (r *avsCheckRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	code := strings.ToUpper(strings.TrimSpace(txn.Metadata["avs_response"]))
	if code == "" {
		return nil
	}
	switch code {
	case "Y", "X": // full match
		return nil
	case "N": // mismatch
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name,
			Decision: parseDecision(r.cfg.StrictDecision, engine.Deny),
			Force:    true,
			Detail:   "AVS=N: address + zip mismatch",
		}
	case "A", "Z":
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name,
			Decision: parseDecision(r.cfg.PartialDecision, engine.Review),
			Detail:   "AVS=" + code + ": partial match",
		}
	}
	return nil // U / 未知 → skip
}

// ─── bin_country ─────────────────────────────────────────────────
//
// metadata.bin_country = 卡 BIN 解析得到的卡 issuer 国家（payment-core 算）。
// 跟 txn.Country (商户声明的交易国家) 不一致 → 跨境支付，加风险分。
//
// 注意：跨境本身不是欺诈（合法跨境购物常见），但叠加其它信号置信度高。所以
// 默认 review-weight 不直接 deny，由 score matrix 累加裁决。

type BINCountryConfig struct {
	Decision string `json:"decision"` // review / deny；默认 review
	Weight   int    `json:"weight"`   // 默认 10
}

type binCountryRule struct {
	id, name string
	enabled  bool
	cfg      BINCountryConfig
	verdict  engine.Decision
}

func BINCountryFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg BINCountryConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("bin_country: %w", err)
			}
		}
		if cfg.Weight <= 0 {
			cfg.Weight = 10
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &binCountryRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v}, nil
	}
}

func (r *binCountryRule) ID() string    { return r.id }
func (r *binCountryRule) Name() string  { return r.name }
func (r *binCountryRule) Type() string  { return "bin_country" }
func (r *binCountryRule) Enabled() bool { return r.enabled }
func (r *binCountryRule) Weight() int   { return r.cfg.Weight }

func (r *binCountryRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	binCountry := strings.ToUpper(strings.TrimSpace(txn.Metadata["bin_country"]))
	if binCountry == "" {
		return nil
	}
	merchantCountry := strings.ToUpper(txn.Country)
	if merchantCountry == "" || binCountry == merchantCountry {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("BIN country %s != merchant country %s (cross-border)",
			binCountry, merchantCountry),
	}
}

// ─── email_validation ────────────────────────────────────────────
//
// 输入：metadata.email_disposable (true/false)、metadata.email_age_days (int)。
// 由 payment-core 在调 Screen 前请求外部服务 (ZeroBounce / Kickbox / 自建) 算好。
//
// 命中：
//   - email_disposable=true → review-weight (一次性邮箱常用 fraud)
//   - email_age_days < min_age → review-weight (新注册邮箱)

type EmailValidationConfig struct {
	BlockDisposable bool   `json:"block_disposable"` // true = 一次性邮箱命中 deny
	MinAgeDays      int    `json:"min_age_days"`     // 默认 7
	Weight          int    `json:"weight"`           // 默认 10
	Decision        string `json:"decision"`         // 默认 review
}

type emailValidationRule struct {
	id, name string
	enabled  bool
	cfg      EmailValidationConfig
	verdict  engine.Decision
}

func EmailValidationFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg EmailValidationConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("email_validation: %w", err)
			}
		}
		if cfg.MinAgeDays <= 0 {
			cfg.MinAgeDays = 7
		}
		if cfg.Weight <= 0 {
			cfg.Weight = 10
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &emailValidationRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v}, nil
	}
}

func (r *emailValidationRule) ID() string    { return r.id }
func (r *emailValidationRule) Name() string  { return r.name }
func (r *emailValidationRule) Type() string  { return "email_validation" }
func (r *emailValidationRule) Enabled() bool { return r.enabled }
func (r *emailValidationRule) Weight() int   { return r.cfg.Weight }

func (r *emailValidationRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	if txn.Metadata["email_disposable"] == "true" {
		dec := r.verdict
		force := false
		if r.cfg.BlockDisposable {
			dec = engine.Deny
			force = true
		}
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: dec, Force: force,
			Detail: "disposable email detected",
		}
	}
	age, _ := strconv.Atoi(txn.Metadata["email_age_days"])
	if age > 0 && age < r.cfg.MinAgeDays {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: r.verdict,
			Detail: fmt.Sprintf("email age %dd < min %dd (new account)", age, r.cfg.MinAgeDays),
		}
	}
	return nil
}

// parseDecision 把字符串 "deny"/"review" 转成 Decision，invalid 用 fallback。
func parseDecision(s string, fallback engine.Decision) engine.Decision {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "deny":
		return engine.Deny
	case "review":
		return engine.Review
	case "allow":
		return engine.Allow
	}
	return fallback
}

// 占位 import：留 store 给将来的 returning_customer 直接查 LinkStore 实现，
// 当前用 metadata 路径，store 暂未直接用。
var _ = store.NewMemLinkStore
