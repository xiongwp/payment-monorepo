// Package engine — per-merchant policy override.
//
// 商业化关键能力：让不同商户配不同阈值 / 关闭部分规则 / 调整 weight。
// 全局 ScoreThresholds 是 baseline；MerchantPolicy 在 baseline 上做加法。
//
// 决策时序（Evaluate 内部）：
//
//  1. 取全局 thresholds
//  2. 如果 PolicyStore != nil 且 txn.MerchantID 命中 override：
//     - thresholds = 商户专属（review_min / deny_min）
//     - rule.Enabled 同时还要 not in disabled_rules
//     - rule.Weight 被 weight_overrides[ruleID] 覆盖
//  3. 评估 → 累加 score → 映射 verdict
//
// PolicyStore 接口刻意保持简单：Get(merchantID) 返回 *MerchantPolicy（不存在
// 返回 nil，无 override 等价 baseline）。生产 PG 实现按 (merchant_id) 索引
// 一张 `risk_merchant_policy` 表，配 cache + invalidation。

package engine

// MerchantPolicy 单个商户的风控策略覆盖。所有字段都是可选；零值 = 不覆盖。
type MerchantPolicy struct {
	// MerchantID 商户唯一 ID。Get(merchantID).MerchantID 必须与查询 key 一致。
	MerchantID string `json:"merchant_id"`
	// ReviewMin / DenyMin 商户自定义阈值。0 → 用 baseline。
	// 例：高风险行业（gambling / crypto）可以调低 ReviewMin 让更多 case 进 review。
	ReviewMin int `json:"review_min,omitempty"`
	DenyMin   int `json:"deny_min,omitempty"`
	// DisabledRules 商户主动关闭的规则 ID 列表（如商户已经自行做卡号校验，
	// 关掉 risk-manage 的对应规则避免双重拦截）。
	DisabledRules []string `json:"disabled_rules,omitempty"`
	// WeightOverrides 商户专属规则权重。例：高风险商户把 ip_risk 从 30 提到 60。
	WeightOverrides map[string]int `json:"weight_overrides,omitempty"`
}

// PolicyStore 商户策略查询接口。Get 返回 nil 表示"无 override"。
type PolicyStore interface {
	Get(merchantID string) *MerchantPolicy
}

// SetPolicyStore 注入策略源。生产侧从 PG 读 + 内存 cache + admin invalidate。
func (e *Engine) SetPolicyStore(p PolicyStore) {
	e.mu.Lock()
	e.policy = p
	e.mu.Unlock()
}

// effectivePolicy 取 merchantID 对应的 override（nil 表示纯 baseline）。
// 同时持锁让 Evaluate 在 Set 期间也读到一致快照。
func (e *Engine) effectivePolicy(merchantID string) *MerchantPolicy {
	if merchantID == "" {
		return nil
	}
	e.mu.RLock()
	p := e.policy
	e.mu.RUnlock()
	if p == nil {
		return nil
	}
	return p.Get(merchantID)
}

// MemPolicyStore 内存版（test / dev / 小规模商户量）。
// 生产换成 PG-backed loader + LRU cache + admin invalidate。
type MemPolicyStore struct {
	policies map[string]*MerchantPolicy
}

func NewMemPolicyStore(policies map[string]*MerchantPolicy) *MemPolicyStore {
	if policies == nil {
		policies = make(map[string]*MerchantPolicy)
	}
	return &MemPolicyStore{policies: policies}
}

func (s *MemPolicyStore) Get(merchantID string) *MerchantPolicy {
	return s.policies[merchantID]
}

// Set / Delete 给 admin / test 用；并发场景生产实现需加锁。
func (s *MemPolicyStore) Set(p *MerchantPolicy) {
	if p == nil || p.MerchantID == "" {
		return
	}
	s.policies[p.MerchantID] = p
}

func (s *MemPolicyStore) Delete(merchantID string) { delete(s.policies, merchantID) }
