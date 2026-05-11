// Package feerule — 费率规则求值引擎。
//
// 输入：一组 FeeRule + 一笔 TransactionInput
// 输出：命中的 rule（最高 priority 的匹配）+ 计算出的 fee minor unit
//
// 求值逻辑（步骤）：
//
//	1. 过滤未生效 / 已过期 / inactive 的 rules
//	2. 按 priority desc 排
//	3. 每条 rule 跑 match()：merchant_id / tier / product / channel / region /
//	    currency / card_bin / amount range 全匹配（空值 = 通配）
//	4. 第一个通过的就是命中 rule
//	5. 用 rule 算 fee：fee = max(min, min(max, percent_bps×amount/10000 + fixed))
//	6. 跨币种再加 fx_markup_bps（独立 line item，主 fee 不含）
//	7. event_type=refund 时按 refund_fee_behavior 处理:
//	      "refund":   fee_minor = -原 fee (退给商户)
//	      "keep":     fee_minor = 0 (商户不退 fee)
//	      "prorate":  按 refund/charge 比例退
//
// 复杂度 O(n) — n = 规则总数。规则量级几百级别，OK。生产可加 cache。

package feerule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"reconcile-system/packages/billing-system/internal/domain"
)

// Engine 规则引擎。
type Engine struct {
	rules []domain.FeeRule
}

// New 构造时按 priority desc 排好。
func New(rules []domain.FeeRule) *Engine {
	cp := make([]domain.FeeRule, len(rules))
	copy(cp, rules)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Priority > cp[j].Priority })
	return &Engine{rules: cp}
}

// Pick 找命中的 rule。返 nil 表示没匹配（caller 应用 default 兜底）。
func (e *Engine) Pick(now time.Time, in domain.TransactionInput) *domain.FeeRule {
	for i := range e.rules {
		r := &e.rules[i]
		if !r.Active {
			continue
		}
		if !now.IsZero() {
			if !r.EffectiveFrom.IsZero() && now.Before(r.EffectiveFrom) {
				continue
			}
			if r.EffectiveTo != nil && now.After(*r.EffectiveTo) {
				continue
			}
		}
		if !matchRule(r, in) {
			continue
		}
		return r
	}
	return nil
}

// matchRule 单条 rule × 单笔交易是否命中（所有非空字段都要对上）。
func matchRule(r *domain.FeeRule, in domain.TransactionInput) bool {
	if r.MerchantID != "" && r.MerchantID != in.MerchantID {
		return false
	}
	if r.MerchantTier != "" && r.MerchantTier != in.MerchantTier {
		return false
	}
	if r.Product != "" && r.Product != in.Product {
		return false
	}
	if r.ChannelAdapter != "" && r.ChannelAdapter != in.ChannelAdapter {
		return false
	}
	if r.Region != "" && r.Region != in.Region {
		return false
	}
	if r.CurrencyAllow != "" && !inCSV(r.CurrencyAllow, in.Currency) {
		return false
	}
	if r.CardBINRange != "" && !inBINRange(r.CardBINRange, in.CardBIN) {
		return false
	}
	if r.AmountMinMinor > 0 && in.AmountMinor < r.AmountMinMinor {
		return false
	}
	if r.AmountMaxMinor > 0 && in.AmountMinor > r.AmountMaxMinor {
		return false
	}
	return true
}

// Compute 算 fee_minor。返 (fee_minor, fx_markup_minor, error)。
//
// fee_minor = clamp(percent_bps × amount / 10000 + fixed, fee_min, fee_max)
// fx_markup_minor 单独返 — caller 落 fee_event 时建议拆两条 line item
// （event_type=charge fee + event_type=fx_spread）让账单清晰。
//
// 同币种时 fx_markup = 0；跨币种时 fx_markup = percent(fx_markup_bps) × amount。
func Compute(r *domain.FeeRule, in domain.TransactionInput, baseCurrency string) (feeMinor, fxMarkupMinor int64, err error) {
	if r == nil {
		return 0, 0, fmt.Errorf("no rule matched")
	}
	amt := in.AmountMinor
	if amt < 0 {
		amt = -amt // refund 时 input 也用正数算，符号由 caller 处理
	}
	// 主 fee
	percentPart := amt * int64(r.PercentBPS) / 10000
	feeMinor = percentPart + r.FixedMinor
	if r.FeeMinMinor > 0 && feeMinor < r.FeeMinMinor {
		feeMinor = r.FeeMinMinor
	}
	if r.FeeMaxMinor > 0 && feeMinor > r.FeeMaxMinor {
		feeMinor = r.FeeMaxMinor
	}
	// FX markup（跨币种附加）
	if r.FXMarkupBPS > 0 && !strings.EqualFold(in.Currency, baseCurrency) {
		fxMarkupMinor = amt * int64(r.FXMarkupBPS) / 10000
	}
	return feeMinor, fxMarkupMinor, nil
}

// ApplyRefundPolicy 给 refund 事件按 rule.RefundFeeBehavior 算返还方向。
//
// originalFeeMinor 是原 charge 的 fee（caller 从历史 fee_event 查回来）。
// refundAmountMinor 是本次退款金额，chargeAmountMinor 是原 charge 金额。
//
// 返：fee_minor (有符号 — 负数 = 退给商户，正数 = 仍收 / 罚)。
func ApplyRefundPolicy(r *domain.FeeRule, originalFeeMinor, refundAmountMinor, chargeAmountMinor int64) int64 {
	if r == nil {
		return 0
	}
	switch r.RefundFeeBehavior {
	case "refund":
		// 完全退给商户
		if refundAmountMinor >= chargeAmountMinor {
			return -originalFeeMinor
		}
		// 部分退也全 fee 退（少见，常见是 prorate）
		return -originalFeeMinor
	case "keep", "":
		// 商户不退 fee（默认 — 商家承担退款手续费）
		return 0
	case "prorate":
		// 按比例退
		if chargeAmountMinor <= 0 {
			return 0
		}
		return -(originalFeeMinor * refundAmountMinor / chargeAmountMinor)
	}
	return 0
}

// ─── helpers ────────────────────────────────────────────────────────

// inCSV "USD,PHP,SGD" 中是否含 currency（大小写不敏感）。
func inCSV(csv, want string) bool {
	want = strings.ToUpper(strings.TrimSpace(want))
	for _, s := range strings.Split(csv, ",") {
		if strings.ToUpper(strings.TrimSpace(s)) == want {
			return true
		}
	}
	return false
}

// inBINRange "4000-4999" 是否含 BIN（取 BIN 前 4-6 位）。
// 多段用 "4000-4999;5100-5199" 分号分隔。
func inBINRange(spec, bin string) bool {
	if bin == "" {
		return false
	}
	for _, seg := range strings.Split(spec, ";") {
		seg = strings.TrimSpace(seg)
		parts := strings.Split(seg, "-")
		if len(parts) != 2 {
			continue
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil {
			continue
		}
		// BIN 一般 6 位；取前 min(6, len(bin)) 位比较
		take := len(parts[0])
		if take > len(bin) {
			take = len(bin)
		}
		n, err := strconv.Atoi(bin[:take])
		if err != nil {
			continue
		}
		if n >= lo && n <= hi {
			return true
		}
	}
	return false
}
