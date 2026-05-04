package rules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/merchantlist"
)

// ─── merchant_allowlist / merchant_blocklist ─────────────────────────
//
// 跟全局 blacklist 区别：merchantlist 是商户独立维护的"我自己的好人 / 坏人"。
//
// 评估每个 dimension：customer / device / ip / card_fingerprint / email。
// 检查的 value 来自 TxnContext 对应字段：
//
//	customer          → CustomerID
//	device            → DeviceID
//	ip                → IPAddress
//	card_fingerprint  → Metadata["card_fingerprint"]  （由 payment-core 在调
//	                    Screen 前算好）
//	email             → Metadata["email"]
//
// 命中行为：
//   - allowlist hit → Hit.Force=true + Decision=Allow → 引擎短路返回 ALLOW
//   - blocklist hit → Hit.Force=true + Decision=Deny  → 引擎短路返回 DENY
//
// 配置：本规则不需要任何 config，只需 type + enabled。merchantlist 数据由
// admin / 商户 API 维护。建议把这两条规则加在 rule list **最前面**让它们最
// 早求值（虽然 Force 短路会跳过后续，但 Worse() / 累加 score 的开销也省了）。
//
// rules.yaml：
//
//	- id: m_allowlist
//	  name: "商户允许名单"
//	  type: merchant_allowlist
//	  enabled: true
//	- id: m_blocklist
//	  name: "商户拒绝名单"
//	  type: merchant_blocklist
//	  enabled: true

type merchantListRule struct {
	id, name string
	enabled  bool
	svc      merchantlist.Service
	kind     merchantlist.Kind
}

// MerchantAllowlistFactory 商户级 allowlist 规则。
func MerchantAllowlistFactory(svc merchantlist.Service) engine.RuleFactory {
	return func(id, name string, enabled bool, _ json.RawMessage) (engine.Rule, error) {
		return &merchantListRule{id: id, name: name, enabled: enabled, svc: svc, kind: merchantlist.KindAllow}, nil
	}
}

// MerchantBlocklistFactory 商户级 blocklist 规则。
func MerchantBlocklistFactory(svc merchantlist.Service) engine.RuleFactory {
	return func(id, name string, enabled bool, _ json.RawMessage) (engine.Rule, error) {
		return &merchantListRule{id: id, name: name, enabled: enabled, svc: svc, kind: merchantlist.KindBlock}, nil
	}
}

func (r *merchantListRule) ID() string    { return r.id }
func (r *merchantListRule) Name() string  { return r.name }
func (r *merchantListRule) Enabled() bool { return r.enabled }

func (r *merchantListRule) Type() string {
	if r.kind == merchantlist.KindAllow {
		return "merchant_allowlist"
	}
	return "merchant_blocklist"
}

func (r *merchantListRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.svc == nil || txn == nil || txn.MerchantID == "" {
		return nil
	}
	checks := []struct {
		dim, val string
	}{
		{"customer", txn.CustomerID},
		{"device", txn.DeviceID},
		{"ip", txn.IPAddress},
	}
	if v := txn.Metadata["card_fingerprint"]; v != "" {
		checks = append(checks, struct{ dim, val string }{"card_fingerprint", v})
	}
	if v := txn.Metadata["email"]; v != "" {
		checks = append(checks, struct{ dim, val string }{"email", v})
	}

	for _, c := range checks {
		if c.val == "" {
			continue
		}
		if e, ok := r.svc.Contains(ctx, txn.MerchantID, r.kind, c.dim, c.val); ok {
			h := &engine.Hit{
				RuleID:   r.id,
				RuleName: r.name,
				Force:    true,
				Detail:   fmt.Sprintf("merchant %s %s hit on %s=%s (reason=%s)", txn.MerchantID, r.kind, c.dim, c.val, e.Reason),
			}
			if r.kind == merchantlist.KindAllow {
				h.Decision = engine.Allow
			} else {
				h.Decision = engine.Deny
			}
			return h
		}
	}
	return nil
}
