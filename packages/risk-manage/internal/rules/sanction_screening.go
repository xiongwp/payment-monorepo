package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/sanction"
)

// ─── sanction_screening: AML / 制裁名单筛查 ────────────────────────
//
// 合规硬要求：命中即必须 DENY，不允许运营 override。
//
// 数据源：metadata.full_name + metadata.country (ISO-2)。商户 / payment-core
// 在调 Screen 前应该把客户登记的法定姓名 + 居住国 塞 metadata。当前规则
// 在 metadata 不全时**不命中**（但记 log 让运营注意 — 商户应整改）。
//
// 配置：
//
//	type: sanction_screening
//	config:
//	  required:    true        # true = metadata 必须有 full_name；缺即视为 review（"未筛查"风险）
//	  decision:    deny        # deny（默认；合规要求）/ review（用于初期接入观察）

type SanctionScreeningConfig struct {
	Required bool   `json:"required"` // true = 缺 full_name 视为 review；false = 缺则 skip
	Decision string `json:"decision"` // deny / review；默认 deny
}

type sanctionScreeningRule struct {
	id, name string
	enabled  bool
	cfg      SanctionScreeningConfig
	svc      sanction.Service
	verdict  engine.Decision
}

func SanctionScreeningFactory(svc sanction.Service) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg SanctionScreeningConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("sanction_screening config: %w", err)
			}
		}
		verdict := engine.Deny
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "review") {
			verdict = engine.Review
		}
		return &sanctionScreeningRule{
			id: id, name: name, enabled: enabled, cfg: cfg, svc: svc, verdict: verdict,
		}, nil
	}
}

func (r *sanctionScreeningRule) ID() string    { return r.id }
func (r *sanctionScreeningRule) Name() string  { return r.name }
func (r *sanctionScreeningRule) Type() string  { return "sanction_screening" }
func (r *sanctionScreeningRule) Enabled() bool { return r.enabled }

func (r *sanctionScreeningRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.svc == nil || txn == nil {
		return nil
	}
	fullName := strings.TrimSpace(txn.Metadata["full_name"])
	if fullName == "" {
		// 商户没传 full_name → 无法筛查
		// required=true: 视为可疑（review），让合规团队补全数据
		// required=false: skip（部分商户场景没法律姓名信息，比如游戏点卡）
		if !r.cfg.Required {
			return nil
		}
		return &engine.Hit{
			RuleID:   r.id,
			RuleName: r.name,
			Decision: engine.Review,
			Detail:   "sanction screening REQUIRED but metadata.full_name missing",
			// Force=true 确保 review，不被其它规则降级到 ALLOW
			Force: true,
		}
	}
	country := strings.ToUpper(strings.TrimSpace(txn.Metadata["country"]))
	if country == "" {
		country = strings.ToUpper(txn.Country)
	}
	m := r.svc.Check(ctx, fullName, country)
	if m == nil || len(m.Hits) == 0 {
		return nil
	}
	// 命中：合规级 DENY + Force 短路（绕过其它规则的 score 累加）
	parts := make([]string, 0, len(m.Hits))
	for _, h := range m.Hits {
		parts = append(parts, fmt.Sprintf("%s/%s(%s)", h.Source, h.UID, h.Name))
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Force:    true,
		Detail:   "SANCTION HIT: " + strings.Join(parts, ", "),
	}
}
