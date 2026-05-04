package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── graph_reputation: 图风险传播 ──────────────────────────────────
//
// 核心思想（Stripe Radar 同款）：把图节点贴上 fraud / chargeback / trusted
// 等 tag，N 跳内的邻居自动继承部分风险 — 离 fraud 越近，风险越高。
//
// 数据源（谁打 tag）：
//   1. service.Report(payment.fraud) → 自动 tag customer + device + ip 为 "fraud"
//   2. dispute / chargeback webhook → tag customer 为 "chargeback"
//   3. review queue.Decide(reject) → tag 为 "review_rejected"
//   4. 商户 blocklist hit → tag 为 "merchant_blocked"
//
// 评估时（本规则）：
//   - 拿 txn 关键节点（customer / device / ip）的 N 跳 tag 集
//   - 按 tag 配置的 weight 累加：fraud=30, chargeback=20, review_rejected=15
//   - 距离衰减：1 跳 100%, 2 跳 50%, 3 跳 25% 衰减（hop_decay 可配）
//   - 累计 score >= threshold 命中 → REVIEW；显著高 → DENY
//
// 配置：
//
//	type: graph_reputation
//	config:
//	  hops:           2
//	  hop_decay:      0.5         # 每多 1 跳 weight 乘此系数
//	  pivots:         ["customer","device","ip"]   # 从哪些节点出发，分别 BFS
//	  tag_weights:
//	    fraud:            30
//	    chargeback:       20
//	    review_rejected:  15
//	  review_min:     20
//	  deny_min:       50
//
// 注意：
//   - 距离衰减是 *per-tag-instance* 的，多个邻居同 tag 各自衰减后累加
//   - hop_decay 由 LinkStore 实现："a 直接邻居"权重高，"邻居的邻居"次之
//     当前 Mem 实现不返回距离，规则内做近似：先 1 跳得 set1，再 2 跳得 set2，
//     set2 - set1 = 严格 2 跳邻居（最优实现等图库返回带距离）

type GraphReputationConfig struct {
	Hops        int                `json:"hops"`         // 默认 2
	HopDecay    float64            `json:"hop_decay"`    // 默认 0.5
	Pivots      []string           `json:"pivots"`       // 默认 ["customer","device","ip"]
	TagWeights  map[string]float64 `json:"tag_weights"`  // 必填
	ReviewMin   float64            `json:"review_min"`   // 默认 20
	DenyMin     float64            `json:"deny_min"`     // 默认 50
}

type graphReputationRule struct {
	id, name string
	enabled  bool
	cfg      GraphReputationConfig
	links    store.LinkStore
}

func GraphReputationFactory(linkStore store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg GraphReputationConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("graph_reputation config: %w", err)
		}
		if cfg.Hops <= 0 {
			cfg.Hops = 2
		}
		if cfg.Hops > store.BFSMaxHops {
			cfg.Hops = store.BFSMaxHops
		}
		if cfg.HopDecay <= 0 || cfg.HopDecay > 1 {
			cfg.HopDecay = 0.5
		}
		if len(cfg.Pivots) == 0 {
			cfg.Pivots = []string{"customer", "device", "ip"}
		}
		if len(cfg.TagWeights) == 0 {
			return nil, fmt.Errorf("graph_reputation: tag_weights required")
		}
		// normalize tag keys lower
		nw := make(map[string]float64, len(cfg.TagWeights))
		for k, v := range cfg.TagWeights {
			nw[strings.ToLower(strings.TrimSpace(k))] = v
		}
		cfg.TagWeights = nw
		if cfg.ReviewMin <= 0 {
			cfg.ReviewMin = 20
		}
		if cfg.DenyMin <= 0 {
			cfg.DenyMin = 50
		}
		return &graphReputationRule{
			id: id, name: name, enabled: enabled, cfg: cfg, links: linkStore,
		}, nil
	}
}

func (r *graphReputationRule) ID() string    { return r.id }
func (r *graphReputationRule) Name() string  { return r.name }
func (r *graphReputationRule) Type() string  { return "graph_reputation" }
func (r *graphReputationRule) Enabled() bool { return r.enabled }

func (r *graphReputationRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil || txn == nil {
		return nil
	}

	totalScore := 0.0
	type contribution struct {
		pivot string
		hop   int
		tag   string
		count int
	}
	var contribs []contribution

	for _, p := range r.cfg.Pivots {
		key := pivotValue(p, txn)
		if key == "" {
			continue
		}
		// 分跳累计：先求 hop=1 set，再求每 hop 增量 set，每个 hop 套衰减系数
		seen := map[string]struct{}{key: {}}
		decay := 1.0
		for hop := 1; hop <= r.cfg.Hops; hop++ {
			tagsAll := r.links.TagsWithin(ctx, key, hop)
			// 把已经在 seen 里的不再算（去重）
			peersThisHop := r.links.PeersWithin(ctx, key, hop, "")
			for _, n := range peersThisHop {
				if _, ok := seen[n]; ok {
					continue
				}
				seen[n] = struct{}{}
			}
			// 简化：本 hop 的 tag count 用 (累计 hops-内 tag count) - (已统计) 近似。
			// Mem 实现没暴露"严格 hop=k"接口，这里用差值法。
			// 近似带来的偏差 = 路径数 vs 节点数（节点可能被多条路径计数；接受）
			usedTags := map[string]int{}
			if hop > 1 {
				prev := r.links.TagsWithin(ctx, key, hop-1)
				for t, c := range prev {
					usedTags[t] = c
				}
			}
			for t, c := range tagsAll {
				delta := c - usedTags[t]
				if delta <= 0 {
					continue
				}
				w, ok := r.cfg.TagWeights[t]
				if !ok {
					continue
				}
				contribScore := float64(delta) * w * decay
				totalScore += contribScore
				contribs = append(contribs, contribution{pivot: p, hop: hop, tag: t, count: delta})
			}
			decay *= r.cfg.HopDecay
		}
	}

	if totalScore < r.cfg.ReviewMin {
		return nil
	}
	verdict := engine.Review
	if totalScore >= r.cfg.DenyMin {
		verdict = engine.Deny
	}
	// 排序输出，便于运营看
	sort.Slice(contribs, func(i, j int) bool {
		return contribs[i].hop < contribs[j].hop ||
			(contribs[i].hop == contribs[j].hop && contribs[i].tag < contribs[j].tag)
	})
	parts := make([]string, 0, len(contribs))
	for _, c := range contribs {
		parts = append(parts, fmt.Sprintf("%s@hop%d×%d=%s", c.tag, c.hop, c.count, c.pivot))
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: verdict,
		Detail:   fmt.Sprintf("graph reputation score=%.1f (%s)", totalScore, strings.Join(parts, ", ")),
	}
}
