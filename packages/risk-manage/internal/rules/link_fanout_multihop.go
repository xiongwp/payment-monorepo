package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── link_fanout_multihop: 多跳 fraud ring 检测 ─────────────────────
//
// 跟 link_fanout 区别：那条只看 1 跳直接邻居（device → customer 数）；
// 这条做 BFS N 跳，捕获分散式 ring：
//
//	device:A — customer:1 — device:B — customer:2 — device:C ...
//
// 1 跳查不到 device:C 跟 device:A 同环，2 跳一查就出来。商业 fraud ring
// 普遍把 device / IP 拆散到多账号，1 跳有意识规避；多跳是必备能力。
//
// 配置：
//
//	type: link_fanout_multihop
//	config:
//	  pivot:        device            # 起点维度
//	  peer:         customer          # 最终计数维度（中间不限制）
//	  hops:         2                 # BFS 深度，1-3
//	  threshold:    8                 # peer 数 > threshold 触发
//	  decision:     review            # review / deny；默认 review
//
// 注意：
//   - threshold 应 > 1-hop 同 pivot/peer 的阈值（hops 多了自然 peer 多）
//   - hops=3 已接近"全连通块查询"，慢且容易把 hub-node（公共代理 IP）拖进来。
//     建议先 shadow mode 上线观察 false positive 比例
//   - 数据源同 link_fanout（service.Report 写边）

type LinkFanoutMultihopConfig struct {
	Pivot     string `json:"pivot"`
	Peer      string `json:"peer"`
	Hops      int    `json:"hops"`      // BFS 深度
	Threshold int    `json:"threshold"` // peer 数 > 此值才触发
	Decision  string `json:"decision"`
}

type linkFanoutMultihopRule struct {
	id, name string
	enabled  bool
	cfg      LinkFanoutMultihopConfig
	links    store.LinkStore
	verdict  engine.Decision
}

func LinkFanoutMultihopFactory(linkStore store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg LinkFanoutMultihopConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("link_fanout_multihop config: %w", err)
		}
		cfg.Pivot = strings.ToLower(strings.TrimSpace(cfg.Pivot))
		cfg.Peer = strings.ToLower(strings.TrimSpace(cfg.Peer))
		if cfg.Pivot == "" || cfg.Peer == "" {
			return nil, fmt.Errorf("link_fanout_multihop: pivot and peer are required")
		}
		if cfg.Hops <= 0 {
			cfg.Hops = 2
		}
		if cfg.Hops > store.BFSMaxHops {
			cfg.Hops = store.BFSMaxHops
		}
		if cfg.Threshold <= 0 {
			return nil, fmt.Errorf("link_fanout_multihop: threshold must be > 0")
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		return &linkFanoutMultihopRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			links: linkStore, verdict: verdict,
		}, nil
	}
}

func (r *linkFanoutMultihopRule) ID() string    { return r.id }
func (r *linkFanoutMultihopRule) Name() string  { return r.name }
func (r *linkFanoutMultihopRule) Type() string  { return "link_fanout_multihop" }
func (r *linkFanoutMultihopRule) Enabled() bool { return r.enabled }

func (r *linkFanoutMultihopRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil {
		return nil
	}
	pivotKey := pivotValue(r.cfg.Pivot, txn)
	if pivotKey == "" {
		return nil
	}
	peerPrefix := r.cfg.Peer + ":"
	peers := r.links.PeersWithin(ctx, pivotKey, r.cfg.Hops, peerPrefix)
	if len(peers) <= r.cfg.Threshold {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail: fmt.Sprintf("%s %q linked to %d %s peers within %d hops (threshold %d)",
			r.cfg.Pivot, pivotKey, len(peers), r.cfg.Peer, r.cfg.Hops, r.cfg.Threshold),
	}
}
