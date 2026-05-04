// account_age.go + disposable_email.go: 两条针对薅羊毛 / ATO 的新规则类型。
//
// 都依赖 caller (payment-core / user-merchant-core) 在 metadata 里塞预算好的
// 字段，规则只做策略判定，不读 LinkStore / Counter（保持轻量、无 I/O）。
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── account_age ─────────────────────────────────────────────────
//
// 账户年龄过小 + 敏感事件（withdraw / refund / 大额支付）= 福利金 / 注册礼薅羊毛
// 信号。例如新注册 24h 内试图提现 → 大概率账号是被"撸毛"工具注册的。
//
// 输入字段：metadata.account_age_days（caller 传 ceil((now - register_at).days)）。
// 缺字段时本规则不命中（caller 没接入），不阻塞主路径。
//
// 配置：
//
//	type: account_age
//	config:
//	  min_days:       1                   # 必填；< min_days 算"新账号"
//	  apply_events:   ["withdraw","refund","payment.high_value"]
//	  decision:       deny
//
// `apply_events` 空 = 全事件类型；非空 = 只在 txn.EventType 命中时评估。
type AccountAgeConfig struct {
	MinDays     int      `json:"min_days"`
	ApplyEvents []string `json:"apply_events"`
	Decision    string   `json:"decision"`
}

type accountAgeRule struct {
	id, name string
	enabled  bool
	cfg      AccountAgeConfig
	verdict  engine.Decision
	events   map[string]struct{}
}

func AccountAgeFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg AccountAgeConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("account_age: %w", err)
			}
		}
		if cfg.MinDays <= 0 {
			return nil, fmt.Errorf("account_age: min_days required")
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		var ev map[string]struct{}
		if len(cfg.ApplyEvents) > 0 {
			ev = make(map[string]struct{}, len(cfg.ApplyEvents))
			for _, e := range cfg.ApplyEvents {
				ev[strings.ToLower(strings.TrimSpace(e))] = struct{}{}
			}
		}
		return &accountAgeRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v, events: ev}, nil
	}
}

func (r *accountAgeRule) ID() string    { return r.id }
func (r *accountAgeRule) Name() string  { return r.name }
func (r *accountAgeRule) Type() string  { return "account_age" }
func (r *accountAgeRule) Enabled() bool { return r.enabled }

func (r *accountAgeRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	if r.events != nil {
		if _, ok := r.events[strings.ToLower(txn.EventType)]; !ok {
			return nil
		}
	}
	raw := txn.Metadata["account_age_days"]
	if raw == "" {
		return nil // caller 没传，不能误拦
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 0 {
		return nil
	}
	if days >= r.cfg.MinDays {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("account_age=%dd < min %dd for event %q",
			days, r.cfg.MinDays, txn.EventType),
	}
}

// ─── disposable_email ────────────────────────────────────────────
//
// 一次性邮箱（mailinator / temp-mail / 10minutemail / yopmail / ...）注册 =
// 大概率薅羊毛 / 撞库脚本。维护一份内置 base list，运维可在 yaml `extra_domains`
// 加更多。命中即 deny / review。
//
// 输入：metadata.email_domain（user-merchant-core register handler 已经填了）。
// 大小写不敏感；空字段不命中。
//
// 配置：
//
//	type: disposable_email
//	config:
//	  extra_domains: ["my-temp.com"]
//	  decision: deny
//	  apply_events: ["register","login"]
type DisposableEmailConfig struct {
	ExtraDomains []string `json:"extra_domains"`
	ApplyEvents  []string `json:"apply_events"`
	Decision     string   `json:"decision"`
}

type disposableEmailRule struct {
	id, name string
	enabled  bool
	cfg      DisposableEmailConfig
	verdict  engine.Decision
	domains  map[string]struct{}
	events   map[string]struct{}
}

// builtinDisposableDomains 一次性邮箱常见域名。生产应每月更新（外部 feed
// 同步到 admin DB；当前先内置）。这里挑 30 个高频，覆盖 80%+ throwaway 流量。
var builtinDisposableDomains = []string{
	"mailinator.com", "yopmail.com", "10minutemail.com", "guerrillamail.com",
	"sharklasers.com", "trashmail.com", "tempmail.com", "temp-mail.org",
	"throwawaymail.com", "getnada.com", "maildrop.cc", "fakeinbox.com",
	"tempinbox.com", "mintemail.com", "tempr.email", "discard.email",
	"jetable.org", "mailcatch.com", "spamgourmet.com", "spambox.us",
	"mytrashmail.com", "dispostable.com", "mail-temp.com", "tmpmail.org",
	"emailondeck.com", "moakt.com", "mohmal.com", "harakirimail.com",
	"meltmail.com", "mailnesia.com",
}

func DisposableEmailFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg DisposableEmailConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("disposable_email: %w", err)
			}
		}
		v := engine.Deny
		if strings.EqualFold(cfg.Decision, "review") {
			v = engine.Review
		}
		domains := make(map[string]struct{}, len(builtinDisposableDomains)+len(cfg.ExtraDomains))
		for _, d := range builtinDisposableDomains {
			domains[strings.ToLower(d)] = struct{}{}
		}
		for _, d := range cfg.ExtraDomains {
			domains[strings.ToLower(strings.TrimSpace(d))] = struct{}{}
		}
		var ev map[string]struct{}
		if len(cfg.ApplyEvents) > 0 {
			ev = make(map[string]struct{}, len(cfg.ApplyEvents))
			for _, e := range cfg.ApplyEvents {
				ev[strings.ToLower(strings.TrimSpace(e))] = struct{}{}
			}
		}
		return &disposableEmailRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v, domains: domains, events: ev}, nil
	}
}

func (r *disposableEmailRule) ID() string    { return r.id }
func (r *disposableEmailRule) Name() string  { return r.name }
func (r *disposableEmailRule) Type() string  { return "disposable_email" }
func (r *disposableEmailRule) Enabled() bool { return r.enabled }

func (r *disposableEmailRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	if r.events != nil {
		if _, ok := r.events[strings.ToLower(txn.EventType)]; !ok {
			return nil
		}
	}
	dom := strings.ToLower(strings.TrimSpace(txn.Metadata["email_domain"]))
	if dom == "" {
		return nil
	}
	if _, hit := r.domains[dom]; !hit {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("disposable email domain %q (event %q)", dom, txn.EventType),
	}
}
