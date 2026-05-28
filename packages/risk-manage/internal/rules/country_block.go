package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── country_block: 国家 / 币种 白名单或黑名单 ─────────────────────

type CountryBlockConfig struct {
	Mode       string   `json:"mode"`       // allow / deny（白名单或黑名单）
	Countries  []string `json:"countries"`  // 国家码列表
	Currencies []string `json:"currencies"` // 币种列表（可选）
}

type countryBlockRule struct {
	id, name string
	enabled  bool
	cfg      CountryBlockConfig
	set      map[string]bool
	curSet   map[string]bool
}

func CountryBlockFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg CountryBlockConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		cs := make(map[string]bool, len(cfg.Countries))
		for _, c := range cfg.Countries {
			cs[strings.ToUpper(c)] = true
		}
		cur := make(map[string]bool, len(cfg.Currencies))
		for _, c := range cfg.Currencies {
			cur[strings.ToUpper(c)] = true
		}
		return &countryBlockRule{id: id, name: name, enabled: enabled, cfg: cfg, set: cs, curSet: cur}, nil
	}
}

func (r *countryBlockRule) ID() string    { return r.id }
func (r *countryBlockRule) Name() string  { return r.name }
func (r *countryBlockRule) Type() string  { return "country_block" }
func (r *countryBlockRule) Enabled() bool { return r.enabled }

func (r *countryBlockRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	country := strings.ToUpper(txn.Country)
	currency := strings.ToUpper(txn.Currency)

	blocked := false
	reason := ""

	if len(r.set) > 0 && country != "" {
		inList := r.set[country]
		if r.cfg.Mode == "allow" && !inList {
			blocked = true
			reason = fmt.Sprintf("国家 %s 不在白名单", country)
		} else if r.cfg.Mode == "deny" && inList {
			blocked = true
			reason = fmt.Sprintf("国家 %s 在黑名单", country)
		}
	}

	if !blocked && len(r.curSet) > 0 && currency != "" {
		inList := r.curSet[currency]
		if r.cfg.Mode == "allow" && !inList {
			blocked = true
			reason = fmt.Sprintf("币种 %s 不在白名单", currency)
		} else if r.cfg.Mode == "deny" && inList {
			blocked = true
			reason = fmt.Sprintf("币种 %s 在黑名单", currency)
		}
	}

	if blocked {
		// Force=true: 国家/币种白名单或黑名单属于强策略，命中即生效，
		// 绕过加权聚合 — 不应被其它"投票型"规则的低 score 稀释成 REVIEW/ALLOW。
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: engine.Deny,
			Force:  true,
			Detail: reason,
		}
	}
	return nil
}
