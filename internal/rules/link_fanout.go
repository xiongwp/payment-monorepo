package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── link_fanout: 图谱关联规则（fraud ring 检测） ─────────────────────
//
// 例：1 个 device 关联 ≥ 5 个不同 customer 时触发 review。Stripe Radar /
// Adyen Thorn 都用类似机制识别"一台设备多账号"或"一个 IP 同时在大量账号
// 登录支付"的 fraud ring。
//
// 配置：
//
//	type: link_fanout
//	config:
//	  pivot:     device      # 评估维度：device / ip / customer / merchant
//	  peer:      customer    # 计数的 peer 维度
//	  threshold: 5           # peer 数 > threshold 触发
//	  decision:  review      # review / deny
//
// 数据来源：service.Report 在 payment.succeeded 时把当前 txn 的
// (device,ip,customer) 三对边写入 LinkStore。

// LinkFanoutConfig 规则配置。
type LinkFanoutConfig struct {
	Pivot     string `json:"pivot"`     // device / ip / customer / merchant
	Peer      string `json:"peer"`      // 计数维度
	Threshold int    `json:"threshold"` // peer 数 > threshold 才触发
	Decision  string `json:"decision"`  // review / deny；默认 review
}

type linkFanoutRule struct {
	id, name string
	enabled  bool
	cfg      LinkFanoutConfig
	links    store.LinkStore
	verdict  engine.Decision
}

// LinkFanoutFactory 注册到 engine：engine.RegisterFactory("link_fanout", LinkFanoutFactory(linkStore))。
func LinkFanoutFactory(linkStore store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg LinkFanoutConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("link_fanout config: %w", err)
		}
		cfg.Pivot = strings.ToLower(strings.TrimSpace(cfg.Pivot))
		cfg.Peer = strings.ToLower(strings.TrimSpace(cfg.Peer))
		if cfg.Pivot == "" || cfg.Peer == "" {
			return nil, fmt.Errorf("link_fanout: pivot and peer are required")
		}
		if cfg.Pivot == cfg.Peer {
			return nil, fmt.Errorf("link_fanout: pivot must differ from peer")
		}
		if cfg.Threshold <= 0 {
			return nil, fmt.Errorf("link_fanout: threshold must be > 0")
		}
		v := strings.ToLower(strings.TrimSpace(cfg.Decision))
		verdict := engine.Review
		if v == "deny" {
			verdict = engine.Deny
		}
		return &linkFanoutRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			links:   linkStore,
			verdict: verdict,
		}, nil
	}
}

func (r *linkFanoutRule) ID() string    { return r.id }
func (r *linkFanoutRule) Name() string  { return r.name }
func (r *linkFanoutRule) Type() string  { return "link_fanout" }
func (r *linkFanoutRule) Enabled() bool { return r.enabled }

func (r *linkFanoutRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.links == nil {
		return nil
	}
	pivotKey := pivotValue(r.cfg.Pivot, txn)
	if pivotKey == "" {
		// txn 没填该维度（比如 device_id 缺失）→ 不命中（保守 fail-open）
		return nil
	}
	peerPrefix := r.cfg.Peer + ":"
	peers := r.links.Peers(ctx, pivotKey, peerPrefix)
	if len(peers) <= r.cfg.Threshold {
		return nil
	}
	detail := fmt.Sprintf("%s %q linked to %d %s peers (threshold %d)",
		r.cfg.Pivot, pivotKey, len(peers), r.cfg.Peer, r.cfg.Threshold)
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail:   detail,
	}
}

// pivotValue 把 pivot 维度 + txn 字段 拼成 LinkStore 的 key（"<dim>:<value>"）。
// 维度未填或值为空 → 返回 ""。
func pivotValue(dim string, txn *engine.TxnContext) string {
	if txn == nil {
		return ""
	}
	var v string
	switch dim {
	case "device":
		v = txn.DeviceID
	case "ip":
		v = txn.IPAddress
	case "customer":
		v = txn.CustomerID
	case "merchant":
		v = txn.MerchantID
	default:
		return ""
	}
	if v == "" {
		return ""
	}
	return dim + ":" + v
}
