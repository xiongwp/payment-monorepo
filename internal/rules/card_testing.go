package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── card_testing: 卡测试 / 卡号库批量测试检测 ─────────────────────
//
// fraud 模式：拿到一批 stolen card → 在多个商户的 checkout 上小额尝试，
// 用支付成功 / 失败信号判定哪些卡还活着。典型特征：
//   - 同 customer / email / device / ip 在很短窗口内尝试 N 张不同卡
//   - 每笔金额都很小（< $10）"试卡费"
//   - 失败比例高（坏卡多）— 但本规则不依赖失败标记，只看尝试笔数
//
// 跟 velocity 区别：velocity 数"笔数"；本规则数"distinct 卡数"。同卡
// 重试 100 次 ≠ 卡测试。
//
// 数据源：依赖 LinkStore 已经写过 (customer / email / device / ip) ↔
// card_fingerprint 边。但 service.Report 当前只写 device / ip / customer 之间，
// 没把 card_fingerprint 写入图。本规则要求 service.Report 增加：
//
//	if cardFP := txn.Metadata["card_fingerprint"]; cardFP != "" {
//	    s.links.Link(ctx, "card:"+cardFP, cust)
//	    s.links.Link(ctx, "card:"+cardFP, dev)
//	    s.links.Link(ctx, "card:"+cardFP, ip)
//	}
//
// 评估：拿 txn 的 customer / email / device / ip pivot，分别查 card: 维度
// peer 数；任一 ≥ threshold 即触发。
//
// 配置：
//
//	type: card_testing
//	config:
//	  pivots:    ["customer","device","ip"]
//	  threshold: 3            # 1 小时内 distinct 卡数 ≥ 3 触发
//	  decision:  deny         # 卡测试默认直接 deny（强信号）

type CardTestingConfig struct {
	Pivots    []string `json:"pivots"`
	Threshold int      `json:"threshold"`
	Decision  string   `json:"decision"`
}

type cardTestingRule struct {
	id, name string
	enabled  bool
	cfg      CardTestingConfig
	links    store.LinkStore
	verdict  engine.Decision
}

func CardTestingFactory(linkStore store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg CardTestingConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("card_testing config: %w", err)
		}
		if len(cfg.Pivots) == 0 {
			cfg.Pivots = []string{"customer", "device", "ip"}
		}
		if cfg.Threshold <= 0 {
			cfg.Threshold = 3
		}
		verdict := engine.Deny
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "review") {
			verdict = engine.Review
		}
		return &cardTestingRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			links: linkStore, verdict: verdict,
		}, nil
	}
}

func (r *cardTestingRule) ID() string    { return r.id }
func (r *cardTestingRule) Name() string  { return r.name }
func (r *cardTestingRule) Type() string  { return "card_testing" }
func (r *cardTestingRule) Enabled() bool { return r.enabled }

func (r *cardTestingRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil || txn == nil {
		return nil
	}
	for _, p := range r.cfg.Pivots {
		key := pivotValue(p, txn)
		if key == "" {
			continue
		}
		cards := r.links.Peers(ctx, key, "card:")
		if len(cards) >= r.cfg.Threshold {
			return &engine.Hit{
				RuleID:   r.id,
				RuleName: r.name,
				Decision: r.verdict,
				Detail: fmt.Sprintf("%s %q tried %d distinct cards in window (threshold %d, likely card testing)",
					p, key, len(cards), r.cfg.Threshold),
			}
		}
	}
	// pivot 没 enough card → 不命中（但当前 txn 自身也算 1 张卡，所以最少 1+0 hits 就触发；
	// 调用方期望"已经试过 threshold-1 张卡"的下一笔会被这条规则拦下，需要 service.Report
	// 对每笔成功 / 失败都把 card_fingerprint 边写入图）
	return nil
}
