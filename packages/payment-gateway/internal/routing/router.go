// Package routing — 智能支付路由。
//
// 商户接 SDK 不指定具体通道；gateway 根据多维信号选最优通道:
//
//   - 金额 → 大额走低费率通道
//   - 国家/币种 → 本币本国通道首选
//   - BIN range → 卡组（Visa/MC/Amex/JCB/UnionPay）匹配
//   - 商户偏好 → 商户预设的优先级
//   - 通道健康度 → 实时成功率 / 延迟（feedback loop）
//   - canary → A/B test 路由比例
//
// 决策规则用 weighted 多目标:
//
//   score = w1×success_rate + w2×(1/fee_bps) + w3×(1/p99_latency) +
//           w4×merchant_preference + w5×canary_bias
//
// 实时反馈:
//   每次通道返回 → routing.UpdateHealth(channel, success, latency)
//
// 故障转移:
//   主通道失败 → fallback 列表按 score desc 顺序尝试 → 最多 N 跳

package routing

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Channel 一个候选通道。
type Channel struct {
	ID            string   `json:"id"`                  // visa-stripe / mc-adyen / gcash-direct
	Name          string   `json:"name"`
	Products      []string `json:"products"`            // ["card_charge"] / ["wallet"]
	SupportedBINs []string `json:"supported_bins"`      // ["400000-499999"] for Visa
	SupportedCcy  []string `json:"supported_currencies"`
	Regions       []string `json:"regions"`             // ISO-3166 alpha-2
	FeeBPS        int      `json:"fee_bps"`             // 我方拿到的费率（用来做选择，非商户收取）
	MaxAmountMinor int64   `json:"max_amount_minor"`
	MinAmountMinor int64   `json:"min_amount_minor"`
	Priority      int      `json:"priority"`            // 配置层优先级
}

// HealthSnapshot 通道运行时健康。
type HealthSnapshot struct {
	ChannelID    string
	SuccessRate  float64       // 0.0 - 1.0 (滑动 5min 窗)
	P99Latency   time.Duration
	LastFailureAt time.Time
	WindowCount  int
}

// Request 路由决策的输入。
type Request struct {
	MerchantID     string
	AmountMinor    int64
	Currency       string
	Region         string  // 商户/用户所在 country
	CardBIN        string  // 卡 BIN 前 6 位（如果是卡支付）
	Product        string  // card_charge / wallet / bank_transfer
	MerchantPrefs  []string // 商户偏好通道顺序（high to low）
}

// Decision 路由决策结果。
type Decision struct {
	Primary    *Channel `json:"primary"`
	Fallback   []*Channel `json:"fallback"`
	Reasoning  []string `json:"reasoning"`         // human-readable 决策理由（debug 用）
	Scores     map[string]float64 `json:"scores"` // 每通道得分
}

// Router 路由器。
type Router struct {
	mu       sync.RWMutex
	channels []Channel
	health   map[string]*HealthSnapshot
	// 权重 (sum should = 1.0)
	wSuccess  float64
	wFee      float64
	wLatency  float64
	wPref     float64
	wCanary   float64
}

// New 构造，默认权重 sucess 50% / fee 25% / latency 15% / pref 10%。
func New(channels []Channel) *Router {
	return &Router{
		channels: channels,
		health:   map[string]*HealthSnapshot{},
		wSuccess: 0.5,
		wFee:     0.25,
		wLatency: 0.15,
		wPref:    0.10,
		wCanary:  0.0,
	}
}

// SetWeights 调权重（运营 A/B test 用）。
func (r *Router) SetWeights(success, fee, latency, pref, canary float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wSuccess, r.wFee, r.wLatency, r.wPref, r.wCanary = success, fee, latency, pref, canary
}

// UpdateHealth 通道返回后调用，更新滑动健康度。
//
// 简化 — 不存原始 sample，直接 EWMA。生产可换 sliding window counter。
func (r *Router) UpdateHealth(channelID string, success bool, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.health[channelID]
	if !ok {
		h = &HealthSnapshot{ChannelID: channelID, SuccessRate: 1.0}
		r.health[channelID] = h
	}
	const alpha = 0.1 // EWMA 平滑
	v := 0.0
	if success {
		v = 1.0
	}
	h.SuccessRate = alpha*v + (1-alpha)*h.SuccessRate
	// 简化 p99：用 EWMA 跟 max；生产应用 quantile sketch (HDR / t-digest)。
	if latency > h.P99Latency {
		h.P99Latency = latency
	} else {
		h.P99Latency = time.Duration(alpha*float64(latency) + (1-alpha)*float64(h.P99Latency))
	}
	if !success {
		h.LastFailureAt = time.Now()
	}
	h.WindowCount++
}

// Decide 拿一个 Request 算路由决策。
//
// 算法:
//   1. filter 不支持当前 product / currency / region / BIN / amount range 的通道
//   2. 每通道算 score (multi-objective weighted sum)
//   3. 按 score desc 排
//   4. 第一个是 primary，后面前 3 个是 fallback
func (r *Router) Decide(req Request) (*Decision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	candidates := r.filter(req)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no channel matches: product=%s currency=%s region=%s amount=%d bin=%s",
			req.Product, req.Currency, req.Region, req.AmountMinor, req.CardBIN)
	}
	scores := make(map[string]float64, len(candidates))
	reasons := make([]string, 0, len(candidates))
	for i := range candidates {
		c := &candidates[i]
		h := r.health[c.ID]
		s := r.score(c, h, req)
		scores[c.ID] = s
		reasons = append(reasons,
			fmt.Sprintf("%s: score=%.3f (fee=%dbps, success=%.3f, p99=%v)",
				c.ID, s, c.FeeBPS,
				safeFloat(h, func(h *HealthSnapshot) float64 { return h.SuccessRate }, 1.0),
				safeFloat(h, func(h *HealthSnapshot) float64 { return float64(h.P99Latency.Milliseconds()) }, 0.0)))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return scores[candidates[i].ID] > scores[candidates[j].ID]
	})
	primary := &candidates[0]
	var fallback []*Channel
	for i := 1; i < len(candidates) && i <= 3; i++ {
		c := candidates[i]
		fallback = append(fallback, &c)
	}
	return &Decision{
		Primary:   primary,
		Fallback:  fallback,
		Reasoning: reasons,
		Scores:    scores,
	}, nil
}

// filter 过滤候选通道。
func (r *Router) filter(req Request) []Channel {
	out := make([]Channel, 0, len(r.channels))
	for _, c := range r.channels {
		if !containsStr(c.Products, req.Product) {
			continue
		}
		if !containsStr(c.SupportedCcy, req.Currency) {
			continue
		}
		if len(c.Regions) > 0 && !containsStr(c.Regions, req.Region) {
			continue
		}
		if c.MinAmountMinor > 0 && req.AmountMinor < c.MinAmountMinor {
			continue
		}
		if c.MaxAmountMinor > 0 && req.AmountMinor > c.MaxAmountMinor {
			continue
		}
		if req.CardBIN != "" && len(c.SupportedBINs) > 0 && !matchBIN(c.SupportedBINs, req.CardBIN) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// score 多目标加权得分。
func (r *Router) score(c *Channel, h *HealthSnapshot, req Request) float64 {
	successRate := 1.0
	if h != nil && h.WindowCount >= 10 {
		successRate = h.SuccessRate
	}
	// fee_bps 越低越好 — 转成 reward: (10000-fee)/10000，0-1 区间
	feeReward := math.Max(0, float64(10000-c.FeeBPS)/10000.0)
	// p99 latency 越低越好；1s 以下都算好
	latReward := 1.0
	if h != nil && h.P99Latency > 0 {
		latReward = 1.0 / (1.0 + float64(h.P99Latency.Milliseconds())/1000.0)
	}
	// merchant preference — 在 prefs list 越靠前越高
	prefReward := 0.0
	for i, p := range req.MerchantPrefs {
		if p == c.ID {
			prefReward = 1.0 - float64(i)*0.1
			if prefReward < 0 {
				prefReward = 0
			}
			break
		}
	}
	// canary bias — 配置层 priority 字段
	canaryReward := float64(c.Priority) / 100.0
	if canaryReward > 1 {
		canaryReward = 1
	}
	return r.wSuccess*successRate + r.wFee*feeReward +
		r.wLatency*latReward + r.wPref*prefReward + r.wCanary*canaryReward
}

// ─── helpers ────────────────────────────────────────────────────────

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func matchBIN(specs []string, bin string) bool {
	if bin == "" {
		return false
	}
	for _, spec := range specs {
		// "400000-499999" 形式
		var lo, hi int64
		_, err := fmt.Sscanf(spec, "%d-%d", &lo, &hi)
		if err != nil {
			continue
		}
		var n int64
		take := len(spec)/2 // 简化：用 spec 前一半长度截 bin
		if take > len(bin) {
			take = len(bin)
		}
		_, err = fmt.Sscanf(bin[:take], "%d", &n)
		if err != nil {
			continue
		}
		if n >= lo && n <= hi {
			return true
		}
	}
	return false
}

func safeFloat(h *HealthSnapshot, get func(*HealthSnapshot) float64, dflt float64) float64 {
	if h == nil {
		return dflt
	}
	return get(h)
}
