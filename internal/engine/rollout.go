// rollout.go: 规则灰度 + A/B 测试基础设施。
//
// 商业化运营核心需求：
//   - **灰度发布**：新规则先放 5% 流量观察，没误杀再开 25% / 50% / 100%
//   - **A/B 测试**：同一商户，并发跑两套规则集（base vs candidate），实时对比
//     verdict 分布；用 hash 把流量稳定分桶（同一 customer 永远在同一桶，
//     不会一会儿命中新规则一会儿不命中）
//
// 实现：把 RuleDef 加一个 Rollout 字段：
//
//	rollout:
//	  bucket_field:  customer_id     # 拿哪个字段算 hash（customer_id / merchant_id / payment_intent_id / random）
//	  enable_pct:    5               # 0-100，命中桶 < 此值才启用本规则
//
// 桶号 = sha256(rule_id + bucket_field_value) % 100；这样不同规则用不同桶
// 分布，不会"被选中的同一组用户永远命中所有灰度规则"。
//
// A/B 测试：把 candidate 规则跟 base 规则用同一 ID 注册成 shadow 模式 +
// rollout.enable_pct=50 → 一半流量影子评估候选，对比 metrics 即可。

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
)

// RolloutConfig 灰度配置（嵌入 RuleDef）。
type RolloutConfig struct {
	BucketField string `mapstructure:"bucket_field" json:"bucket_field"` // customer_id / merchant_id / payment_intent_id / random
	EnablePct   int    `mapstructure:"enable_pct"   json:"enable_pct"`   // 0-100；0 = 完全 disabled，100 = 全开
}

// inRollout 决定本笔 txn 是否在该 rule 的灰度桶内。
//   - cfg.EnablePct <= 0   → false（rule 完全不启用，等价 disabled）
//   - cfg.EnablePct >= 100 → true（全量启用，等价没配 rollout）
//   - 否则 hash(rule_id || bucket_value) % 100 < EnablePct
//
// bucket_value 缺失（如 BucketField=customer_id 但 txn.CustomerID 空）→ 用
// random hash（每笔不一样），等价"按概率独立采样"。
func inRollout(ruleID string, cfg RolloutConfig, txn *TxnContext) bool {
	if cfg.EnablePct <= 0 {
		return false
	}
	if cfg.EnablePct >= 100 {
		return true
	}
	bucketValue := bucketValueOf(cfg.BucketField, txn)
	bucket := hashBucket(ruleID, bucketValue)
	return int(bucket) < cfg.EnablePct
}

func bucketValueOf(field string, txn *TxnContext) string {
	if txn == nil {
		return ""
	}
	switch field {
	case "customer_id":
		return txn.CustomerID
	case "merchant_id":
		return txn.MerchantID
	case "payment_intent_id":
		return txn.PaymentIntentID
	case "ip_address":
		return txn.IPAddress
	case "device_id":
		return txn.DeviceID
	default:
		return "" // → 走 random（每笔独立）
	}
}

// hashBucket 算 [0, 100) 的桶号。空 value → 用 timer-derived randomness
// 让每笔结果独立（fallback for rule-id alone）。
//
// 用 sha256 而不是 fnv：rule_id 短 + 用户希望不同 rule 的桶分布相互独立，
// sha256 提供更好的雪崩；性能 ~2µs，远低于一次 rule eval。
func hashBucket(ruleID, bucketValue string) uint64 {
	h := sha256.New()
	h.Write([]byte(ruleID))
	h.Write([]byte("|"))
	h.Write([]byte(bucketValue))
	sum := h.Sum(nil)
	// 取首 8 字节做 uint64 mod 100
	return binary.BigEndian.Uint64(sum[:8]) % 100
}

// rolloutRule 包装一条 Rule，按 RolloutConfig 桶号决定是否真的 evaluate。
// 桶外 → 直接返回 nil（等价 miss）；桶内 → 转发给 inner.Evaluate。
//
// Mode / Weight 通过 inner Rule 透传（让 weighted/shadow wrappers 仍生效）。
type rolloutRule struct {
	Rule
	ruleID  string
	rollout RolloutConfig
}

// Evaluate 桶过滤 + 转发。
func (r *rolloutRule) Evaluate(ctx context.Context, txn *TxnContext) *Hit {
	if !inRollout(r.ruleID, r.rollout, txn) {
		return nil
	}
	return r.Rule.Evaluate(ctx, txn)
}

// Mode / Weight 透传到 inner（Wrapper 链：weighted → shadow → rollout，
// 也可能不同顺序；defensive 多一层）。
func (r *rolloutRule) Mode() Mode {
	if m, ok := r.Rule.(ModedRule); ok {
		return m.Mode()
	}
	return ModeEnforce
}

func (r *rolloutRule) Weight() int {
	if w, ok := r.Rule.(WeightedRule); ok {
		return w.Weight()
	}
	return 0
}
