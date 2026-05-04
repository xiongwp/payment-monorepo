// registration_abuse.go: 5 条注册防刷 / 设备真实性核心 Go 规则。
//
// 其他批量注册规则（用户名相似 / 凌晨登录 / 跨城市等）用 DSL 表达，见
// config/rules-recipes.yaml。这里只放需要 store 状态的：
//
//   - register_interval: 同 key (device/ip) 注册间隔 < N 秒（IntervalTracker）
//   - register_burst:    同 IP 在 sub-window 秒级聚集（counter velocity）
//   - username_pattern:  metadata.username 命中 user001 类批量模式
//   - fingerprint_multi_account: 同 fingerprint_hash 关联 ≥ N 账号（link_fanout 套壳）
//   - ua_batch_register: 同 UA hash 关联 ≥ N 账号（同上）
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── register_interval ──────────────────────────────────────────────
//
// "同 key (device/ip) 上一次注册到本次 < interval_seconds 秒" → 批量注册信号。
// 跟 register_velocity (笔数) 区别：本规则关注**最小间隔**，比如同设备
// 5 秒内连发 = bot；笔数也许才 2，velocity 不命中但 interval 命中。

type RegisterIntervalConfig struct {
	Pivot    string `json:"pivot"`    // device / ip
	MaxSec   int    `json:"max_sec"`  // 间隔 < 此值 触发
	Decision string `json:"decision"` // deny / review；默认 deny
}

type registerIntervalRule struct {
	id, name string
	enabled  bool
	cfg      RegisterIntervalConfig
	tracker  store.IntervalTracker
	verdict  engine.Decision
}

func RegisterIntervalFactory(t store.IntervalTracker) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg RegisterIntervalConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("register_interval: %w", err)
			}
		}
		if cfg.Pivot == "" {
			cfg.Pivot = "device"
		}
		if cfg.MaxSec <= 0 {
			cfg.MaxSec = 30
		}
		v := engine.Deny
		if strings.EqualFold(cfg.Decision, "review") {
			v = engine.Review
		}
		return &registerIntervalRule{id: id, name: name, enabled: enabled, cfg: cfg, tracker: t, verdict: v}, nil
	}
}

func (r *registerIntervalRule) ID() string    { return r.id }
func (r *registerIntervalRule) Name() string  { return r.name }
func (r *registerIntervalRule) Type() string  { return "register_interval" }
func (r *registerIntervalRule) Enabled() bool { return r.enabled }

func (r *registerIntervalRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.tracker == nil || txn == nil || txn.EventType != "register" {
		return nil
	}
	pivotKey := pivotValue(r.cfg.Pivot, txn)
	if pivotKey == "" {
		return nil
	}
	key := "register_interval:" + pivotKey
	now := time.Now()
	prev := r.tracker.Record(ctx, key, now)
	if prev.IsZero() {
		return nil // 第一次
	}
	gap := int(now.Sub(prev).Seconds())
	if gap >= r.cfg.MaxSec {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("%s %q register interval %ds < threshold %ds (batch register)",
			r.cfg.Pivot, pivotKey, gap, r.cfg.MaxSec),
	}
}

// ─── register_burst ─────────────────────────────────────────────────
//
// 同 IP / device 在窗口内 register 笔数 ≥ 阈值。已有 register_velocity 等价；
// 提供 type alias 方便 yaml 配置 + 文档对齐。

// （直接复用 RegisterVelocityFactory；不另写 type）

// ─── username_pattern ──────────────────────────────────────────────
//
// metadata.username 命中批量模式：
//   - 末尾连续数字 ≥ N (user001, test123)
//   - 完全数字
//   - 跟其它 N+1 条注册 username 编辑距离 ≤ 1（暂不实现，需要 history 库）
//
// 本规则只做 regex 模式检测；编辑距离做成 LinkStore-based 可后续加。

type UsernamePatternConfig struct {
	MinTrailingDigits int    `json:"min_trailing_digits"` // 默认 3
	BlockNumericOnly  bool   `json:"block_numeric_only"`  // 默认 true：纯数字 username 拦
	Decision          string `json:"decision"`            // deny / review
}

type usernamePatternRule struct {
	id, name string
	enabled  bool
	cfg      UsernamePatternConfig
	verdict  engine.Decision
	digitSuffix *regexp.Regexp
	allDigits   *regexp.Regexp
}

func UsernamePatternFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg UsernamePatternConfig
		cfg.BlockNumericOnly = true
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("username_pattern: %w", err)
			}
		}
		if cfg.MinTrailingDigits <= 0 {
			cfg.MinTrailingDigits = 3
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &usernamePatternRule{
			id: id, name: name, enabled: enabled, cfg: cfg, verdict: v,
			digitSuffix: regexp.MustCompile(fmt.Sprintf(`\d{%d,}$`, cfg.MinTrailingDigits)),
			allDigits:   regexp.MustCompile(`^\d+$`),
		}, nil
	}
}

func (r *usernamePatternRule) ID() string    { return r.id }
func (r *usernamePatternRule) Name() string  { return r.name }
func (r *usernamePatternRule) Type() string  { return "username_pattern" }
func (r *usernamePatternRule) Enabled() bool { return r.enabled }

func (r *usernamePatternRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	username := strings.ToLower(strings.TrimSpace(txn.Metadata["username"]))
	if username == "" {
		return nil
	}
	if r.cfg.BlockNumericOnly && r.allDigits.MatchString(username) {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: r.verdict,
			Detail: fmt.Sprintf("username %q is all-digits (likely batch registration)", username),
		}
	}
	if r.digitSuffix.MatchString(username) {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: r.verdict,
			Detail: fmt.Sprintf("username %q ends with ≥%d digits (batch pattern user001/user002...)",
				username, r.cfg.MinTrailingDigits),
		}
	}
	return nil
}

// ─── fingerprint_multi_account ─────────────────────────────────────
//
// 同 fingerprint_hash 关联 ≥ N 个 customer。同 link_fanout (device→customer)
// 概念但 pivot 是更稳定的 fingerprint_hash（设备 ID 可能变；fingerprint
// 更稳定）。

type FingerprintMultiAccountConfig struct {
	Threshold int    `json:"threshold"` // 默认 5
	Decision  string `json:"decision"`  // 默认 review
}

type fingerprintMultiAccountRule struct {
	id, name string
	enabled  bool
	cfg      FingerprintMultiAccountConfig
	links    store.LinkStore
	verdict  engine.Decision
}

func FingerprintMultiAccountFactory(links store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg FingerprintMultiAccountConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("fingerprint_multi_account: %w", err)
			}
		}
		if cfg.Threshold <= 0 {
			cfg.Threshold = 5
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &fingerprintMultiAccountRule{id: id, name: name, enabled: enabled, cfg: cfg, links: links, verdict: v}, nil
	}
}

func (r *fingerprintMultiAccountRule) ID() string    { return r.id }
func (r *fingerprintMultiAccountRule) Name() string  { return r.name }
func (r *fingerprintMultiAccountRule) Type() string  { return "fingerprint_multi_account" }
func (r *fingerprintMultiAccountRule) Enabled() bool { return r.enabled }

func (r *fingerprintMultiAccountRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil || txn == nil || txn.FingerprintHash == "" {
		return nil
	}
	key := "fp:" + txn.FingerprintHash
	customers := r.links.Peers(ctx, key, "customer:")
	if len(customers) < r.cfg.Threshold {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("fingerprint %q linked to %d customers (threshold %d, batch / shared device)",
			txn.FingerprintHash[:min4(8, len(txn.FingerprintHash))], len(customers), r.cfg.Threshold),
	}
}

// ─── ua_batch_register ─────────────────────────────────────────────
//
// 同 UA hash 关联 ≥ N 个 customer 在 register 事件。批量脚本 UA 完全一致。

type UABatchRegisterConfig struct {
	Threshold int    `json:"threshold"`
	Decision  string `json:"decision"`
}

type uaBatchRegisterRule struct {
	id, name string
	enabled  bool
	cfg      UABatchRegisterConfig
	links    store.LinkStore
	verdict  engine.Decision
}

func UABatchRegisterFactory(links store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg UABatchRegisterConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("ua_batch_register: %w", err)
			}
		}
		if cfg.Threshold <= 0 {
			cfg.Threshold = 10
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &uaBatchRegisterRule{id: id, name: name, enabled: enabled, cfg: cfg, links: links, verdict: v}, nil
	}
}

func (r *uaBatchRegisterRule) ID() string    { return r.id }
func (r *uaBatchRegisterRule) Name() string  { return r.name }
func (r *uaBatchRegisterRule) Type() string  { return "ua_batch_register" }
func (r *uaBatchRegisterRule) Enabled() bool { return r.enabled }

func (r *uaBatchRegisterRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil || txn == nil || txn.EventType != "register" {
		return nil
	}
	uaHash := txn.UAHash
	if uaHash == "" {
		uaHash = simpleHash(txn.UserAgent)
	}
	if uaHash == "" {
		return nil
	}
	key := "ua:" + uaHash
	customers := r.links.Peers(ctx, key, "customer:")
	if len(customers) < r.cfg.Threshold {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("UA hash linked to %d customers in register events (threshold %d, batch script)",
			len(customers), r.cfg.Threshold),
	}
}

// simpleHash UA → 短 hash（不需要 crypto，FNV-1a 32-bit hex 就够）。
func simpleHash(s string) string {
	if s == "" {
		return ""
	}
	const (
		offset uint32 = 2166136261
		prime  uint32 = 16777619
	)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return fmt.Sprintf("%08x", h)
}

func min4(a, b int) int {
	if a < b {
		return a
	}
	return b
}
