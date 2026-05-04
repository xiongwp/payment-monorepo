package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── ip_risk: 基于 IPIntel 富化结果的规则 ───────────────────────────────
//
// 端 SDK 上报 + service 层调 ipintel.Service 后，TxnContext 的 IPCountry /
// IPProxy / IPVPN / IPDataCenter 会被填充。本规则按 yaml 配置选择哪些信号
// 当 fraud 信号。
//
// 配置：
//
//	type: ip_risk
//	config:
//	  block_proxy:        true   # IPProxy=true 命中
//	  block_vpn:          true   # IPVPN=true 命中
//	  block_datacenter:   false  # 数据中心 IP（云厂商）；视场景选择
//	  country_mismatch:   true   # IPCountry 与 txn.Country 不一致命中
//	  decision:           review # review / deny
//
// 至少一个 block_*/country_mismatch 触发即命中（OR）。所有都关 → 规则永远不命中。

type IPRiskConfig struct {
	BlockProxy       bool   `json:"block_proxy"`
	BlockVPN         bool   `json:"block_vpn"`
	BlockDataCenter  bool   `json:"block_datacenter"`
	CountryMismatch  bool   `json:"country_mismatch"`
	Decision         string `json:"decision"` // review / deny；默认 review
}

type ipRiskRule struct {
	id, name string
	enabled  bool
	cfg      IPRiskConfig
	verdict  engine.Decision
}

func IPRiskFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg IPRiskConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("ip_risk config: %w", err)
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		return &ipRiskRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: verdict}, nil
	}
}

func (r *ipRiskRule) ID() string    { return r.id }
func (r *ipRiskRule) Name() string  { return r.name }
func (r *ipRiskRule) Type() string  { return "ip_risk" }
func (r *ipRiskRule) Enabled() bool { return r.enabled }

func (r *ipRiskRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	var reasons []string
	if r.cfg.BlockProxy && txn.IPProxy {
		reasons = append(reasons, "ip is known proxy")
	}
	if r.cfg.BlockVPN && txn.IPVPN {
		reasons = append(reasons, "ip is commercial VPN")
	}
	if r.cfg.BlockDataCenter && txn.IPDataCenter {
		reasons = append(reasons, "ip belongs to data center ASN")
	}
	if r.cfg.CountryMismatch && txn.IPCountry != "" && txn.Country != "" &&
		!strings.EqualFold(txn.IPCountry, txn.Country) {
		reasons = append(reasons, fmt.Sprintf("ip country %q != txn country %q", txn.IPCountry, txn.Country))
	}
	if len(reasons) == 0 {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail:   strings.Join(reasons, "; "),
	}
}
