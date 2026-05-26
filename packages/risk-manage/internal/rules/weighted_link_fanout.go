package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── weighted_link_fanout: 带时间衰减的图谱关联规则 ─────────────────
//
// 跟 link_fanout 区别：那条按"peer 数 > N"硬阈值触发，3 年前 register 的
// device 跟今天的等价；这条按时间衰减加权，老边贡献小，新边贡献大。
//
// 公式：weight_per_edge = e^(-Δt / halflife)，总 fanout = sum(weight_per_edge)。
// halflife=30d 时，30 天前的边权重 = 0.5；90 天前 = 0.125。
//
// 配置：
//
//	type: weighted_link_fanout
//	config:
//	  pivot:          device           # 评估维度
//	  peer:           customer         # 计数维度
//	  hops:           1                # 1 = 等同单跳；2-3 = 多跳带衰减
//	  halflife_days:  30               # 时间半衰期天数，默认 30
//	  hop_decay:      0.5              # 每多 1 跳乘此系数（仅 hops>1 用），默认 0.5
//	  max_weight:     8.0              # 总加权 fanout > max_weight 触发
//	  decision:       review           # review / deny；默认 review
//
// 触发示例：
//   - "今天 1 个 device 关联 10 个新 customer"（10 条 weight≈1 边 → fanout≈10 > 8）→ 命中
//   - "3 年前的 device 上挂了 100 个 customer，最近无新增"（100 × e^(-36) ≈ 0）→ 不命中
//   - 链路欺诈："3 天前关联 5 个 + 30 天前关联 5 个" → 5×0.93 + 5×0.5 ≈ 7.15，逼近阈值
//
// 跟 link_fanout 并存：两条规则都开时取交集（都命中才信号强）；A/B 期可
// shadow 跑此条对比误报率。

type WeightedLinkFanoutConfig struct {
	Pivot        string  `json:"pivot"`
	Peer         string  `json:"peer"`
	Hops         int     `json:"hops"`          // 默认 1
	HalflifeDays float64 `json:"halflife_days"` // 默认 30
	HopDecay     float64 `json:"hop_decay"`     // 默认 0.5；仅 hops>1 生效
	MaxWeight    float64 `json:"max_weight"`    // 加权 fanout > max_weight 触发
	Decision     string  `json:"decision"`
}

type weightedLinkFanoutRule struct {
	id, name string
	enabled  bool
	cfg      WeightedLinkFanoutConfig
	halflife time.Duration
	links    store.LinkStore
	verdict  engine.Decision
}

func WeightedLinkFanoutFactory(linkStore store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg WeightedLinkFanoutConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("weighted_link_fanout config: %w", err)
		}
		cfg.Pivot = strings.ToLower(strings.TrimSpace(cfg.Pivot))
		cfg.Peer = strings.ToLower(strings.TrimSpace(cfg.Peer))
		if cfg.Pivot == "" || cfg.Peer == "" {
			return nil, fmt.Errorf("weighted_link_fanout: pivot and peer are required")
		}
		if cfg.Pivot == cfg.Peer {
			return nil, fmt.Errorf("weighted_link_fanout: pivot must differ from peer")
		}
		if cfg.Hops <= 0 {
			cfg.Hops = 1
		}
		if cfg.Hops > store.BFSMaxHops {
			cfg.Hops = store.BFSMaxHops
		}
		if cfg.HalflifeDays <= 0 {
			cfg.HalflifeDays = 30
		}
		if cfg.HopDecay <= 0 || cfg.HopDecay >= 1 {
			cfg.HopDecay = 0.5
		}
		if cfg.MaxWeight <= 0 {
			return nil, fmt.Errorf("weighted_link_fanout: max_weight must be > 0")
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		halflife := time.Duration(cfg.HalflifeDays * 24 * float64(time.Hour))
		return &weightedLinkFanoutRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			halflife: halflife,
			links:    linkStore,
			verdict:  verdict,
		}, nil
	}
}

func (r *weightedLinkFanoutRule) ID() string    { return r.id }
func (r *weightedLinkFanoutRule) Name() string  { return r.name }
func (r *weightedLinkFanoutRule) Type() string  { return "weighted_link_fanout" }
func (r *weightedLinkFanoutRule) Enabled() bool { return r.enabled }

func (r *weightedLinkFanoutRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil {
		return nil
	}
	pivotKey := pivotValue(r.cfg.Pivot, txn)
	if pivotKey == "" {
		return nil
	}
	peerPrefix := r.cfg.Peer + ":"
	var total float64
	var count int
	if r.cfg.Hops <= 1 {
		total, count = r.links.WeightedFanout(ctx, pivotKey, peerPrefix, r.halflife)
	} else {
		total, count = r.links.WeightedPeersWithin(ctx, pivotKey, r.cfg.Hops, peerPrefix,
			r.halflife, r.cfg.HopDecay)
	}
	if total <= r.cfg.MaxWeight {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail: fmt.Sprintf(
			"%s %q weighted %s fanout=%.2f over %d peers (halflife=%.0fd, hops=%d, max=%.2f)",
			r.cfg.Pivot, pivotKey, r.cfg.Peer, total, count,
			r.cfg.HalflifeDays, r.cfg.Hops, r.cfg.MaxWeight),
	}
}
