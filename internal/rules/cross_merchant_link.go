package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── cross_merchant_link: 跨商户共享检测 ──────────────────────────
//
// 单商户视角看不到的强欺诈信号：同一 device / IP / customer 在 1 小时内
// 出现在 N 个不同商户的成功支付里。正常用户极少在多商户高频切换；这通常
// 是 fraud-as-a-service：同一作弊客户端跑遍若干小商户测卡 / 套现。
//
// 平台级才能做：每个商户单独看，自己只是 "1 个新用户的第 1 笔"，不可疑；
// risk-manage 跨商户聚合才暴露 ring。
//
// 实现思路：
//   - service.Report 在 payment.succeeded 时已经写入 device→customer / ip→customer
//     边到 LinkStore，但**没有写 merchant 维度**。本规则要求 link 时也写
//     device→merchant / ip→merchant 边（Phase 14d 一并改 service.Report）。
//   - 评估：拿 txn 的 device + ip，分别查 PeersWithin(*, "merchant:") 1 跳；
//     去重后总 merchant 数 ≥ threshold → 命中
//
// 配置：
//
//	type: cross_merchant_link
//	config:
//	  pivots:    ["device","ip"]   # 默认两个；可只看 device
//	  threshold: 3                 # 跨 N 个商户触发
//	  decision:  review            # review / deny；默认 review

type CrossMerchantLinkConfig struct {
	Pivots    []string `json:"pivots"`
	Threshold int      `json:"threshold"`
	Decision  string   `json:"decision"`
}

type crossMerchantLinkRule struct {
	id, name string
	enabled  bool
	cfg      CrossMerchantLinkConfig
	links    store.LinkStore
	verdict  engine.Decision
}

func CrossMerchantLinkFactory(linkStore store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg CrossMerchantLinkConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("cross_merchant_link config: %w", err)
		}
		if len(cfg.Pivots) == 0 {
			cfg.Pivots = []string{"device", "ip"}
		}
		if cfg.Threshold <= 0 {
			cfg.Threshold = 3
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		return &crossMerchantLinkRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			links: linkStore, verdict: verdict,
		}, nil
	}
}

func (r *crossMerchantLinkRule) ID() string    { return r.id }
func (r *crossMerchantLinkRule) Name() string  { return r.name }
func (r *crossMerchantLinkRule) Type() string  { return "cross_merchant_link" }
func (r *crossMerchantLinkRule) Enabled() bool { return r.enabled }

func (r *crossMerchantLinkRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil || txn == nil {
		return nil
	}
	merchantSet := map[string]struct{}{}
	for _, p := range r.cfg.Pivots {
		key := pivotValue(p, txn)
		if key == "" {
			continue
		}
		for _, m := range r.links.Peers(ctx, key, "merchant:") {
			merchantSet[m] = struct{}{}
		}
	}
	// 把当前商户也算上（防止 Report 还没把当前 txn 的边写入时漏算）
	if txn.MerchantID != "" {
		merchantSet["merchant:"+txn.MerchantID] = struct{}{}
	}
	if len(merchantSet) < r.cfg.Threshold {
		return nil
	}
	merchants := make([]string, 0, len(merchantSet))
	for m := range merchantSet {
		merchants = append(merchants, m)
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail: fmt.Sprintf("device/ip seen across %d merchants in window (threshold %d): %s",
			len(merchantSet), r.cfg.Threshold, strings.Join(merchants, ",")),
	}
}
