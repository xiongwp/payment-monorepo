package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── blacklist: 商户/用户/IP/设备 黑名单 ────────────────────────────

type BlacklistConfig struct {
	// 维度：merchant / customer / ip / device / card_bin
	Dimension string `json:"dimension"`
}

type blacklistRule struct {
	id, name string
	enabled  bool
	cfg      BlacklistConfig
	bl       store.Blacklist
}

func BlacklistFactory(bl store.Blacklist) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg BlacklistConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return &blacklistRule{id: id, name: name, enabled: enabled, cfg: cfg, bl: bl}, nil
	}
}

func (r *blacklistRule) ID() string    { return r.id }
func (r *blacklistRule) Name() string  { return r.name }
func (r *blacklistRule) Type() string  { return "blacklist" }
func (r *blacklistRule) Enabled() bool { return r.enabled }

func (r *blacklistRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	val := pickDimension(r.cfg.Dimension, txn)
	if val == "" {
		return nil
	}
	if r.bl.Contains(ctx, r.cfg.Dimension, val) {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: engine.Deny,
			Detail: fmt.Sprintf("%s=%s 在黑名单中", r.cfg.Dimension, mask(val)),
		}
	}
	return nil
}

func pickDimension(dim string, txn *engine.TxnContext) string {
	switch dim {
	case "merchant":
		return txn.MerchantID
	case "customer":
		return txn.CustomerID
	case "ip":
		return txn.IPAddress
	case "device":
		return txn.DeviceID
	default:
		return ""
	}
}

// mask 脱敏：只显示前 4 + 后 2 字符
func mask(s string) string {
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "***" + s[len(s)-2:]
}
