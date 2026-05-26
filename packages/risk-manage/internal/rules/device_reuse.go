package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── device_reuse: 设备复用 / "一机多账号" 反向检测 ────────────────────
//
// 跟 link_fanout (pivot=device, peer=customer) 类似，但作为简化的"已聚合好"
// 规则版本：直接读 TxnContext.DeviceDistinctCustomers90d（DeviceGraphExtractor
// 预填），不再依赖运行时 LinkStore 查询，让规则评估更便宜（无锁）。
//
// 场景：
//   - fraud ring 一台设备跑 5-20 个账号洗卡；正常家庭共享一般 1-3 个
//   - 公共终端（网吧 / 酒店大堂）也会 N 大，但 risk-manage 用户多是私域
//     电商，公共终端少见，因此 N > 5 强信号
//
// 也支持反向（customer 用 >N 个不同 device → 撞库 / 凭证盗取信号）；
// 通过 config.dimension 切换。
//
// 配置：
//
//	type: device_reuse
//	config:
//	  dimension: device_customers   # device_customers (默认) / customer_devices
//	  threshold: 5                  # > threshold 触发
//	  decision:  review             # review / deny；默认 review
type DeviceReuseConfig struct {
	Dimension string `json:"dimension"`
	Threshold int    `json:"threshold"`
	Decision  string `json:"decision"`
}

const (
	dimDeviceCustomers = "device_customers"
	dimCustomerDevices = "customer_devices"
)

type deviceReuseRule struct {
	id, name string
	enabled  bool
	cfg      DeviceReuseConfig
	verdict  engine.Decision
}

// DeviceReuseFactory 注册：engine.RegisterFactory("device_reuse", DeviceReuseFactory()).
// 跟 LinkStore 解耦 — 评估只读 typed 字段（DeviceGraphExtractor 预填）。
func DeviceReuseFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg DeviceReuseConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("device_reuse config: %w", err)
			}
		}
		cfg.Dimension = strings.ToLower(strings.TrimSpace(cfg.Dimension))
		if cfg.Dimension == "" {
			cfg.Dimension = dimDeviceCustomers
		}
		if cfg.Dimension != dimDeviceCustomers && cfg.Dimension != dimCustomerDevices {
			return nil, fmt.Errorf("device_reuse: dimension must be %q or %q, got %q",
				dimDeviceCustomers, dimCustomerDevices, cfg.Dimension)
		}
		if cfg.Threshold <= 0 {
			return nil, fmt.Errorf("device_reuse: threshold must be > 0")
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		return &deviceReuseRule{
			id: id, name: name, enabled: enabled, cfg: cfg, verdict: verdict,
		}, nil
	}
}

func (r *deviceReuseRule) ID() string    { return r.id }
func (r *deviceReuseRule) Name() string  { return r.name }
func (r *deviceReuseRule) Type() string  { return "device_reuse" }
func (r *deviceReuseRule) Enabled() bool { return r.enabled }

func (r *deviceReuseRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	var observed int
	var detailVerb string
	switch r.cfg.Dimension {
	case dimDeviceCustomers:
		observed = txn.DeviceDistinctCustomers90d
		detailVerb = "device shared by"
	case dimCustomerDevices:
		observed = txn.CustomerDistinctDevices90d
		detailVerb = "customer used"
	default:
		return nil
	}
	if observed <= r.cfg.Threshold {
		return nil
	}
	suffix := "customers"
	if r.cfg.Dimension == dimCustomerDevices {
		suffix = "devices"
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail: fmt.Sprintf("%s %d distinct %s in 90d (threshold %d)",
			detailVerb, observed, suffix, r.cfg.Threshold),
	}
}
