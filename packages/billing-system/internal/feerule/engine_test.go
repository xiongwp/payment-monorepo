// engine_test.go — fee_rule 引擎单元测试。
//
// 钱相关代码必测。覆盖:
//   - priority desc 命中（高 priority 覆盖低）
//   - 多维匹配（merchant_id / tier / product / channel / region / currency / bin / amount range）
//   - 时效（effective_from / effective_to）
//   - active=false 跳过
//   - Compute fee_min / fee_max clamp
//   - FX markup 跨币种触发，同币种不触发
//   - ApplyRefundPolicy 三策略

package feerule

import (
	"testing"
	"time"

	"reconcile-system/packages/billing-system/internal/domain"
)

func mkRule(id int64, priority int, fields func(*domain.FeeRule)) domain.FeeRule {
	r := domain.FeeRule{
		ID: id, Name: "r" + itoa(int(id)), Priority: priority,
		PercentBPS: 290, FixedMinor: 30, Active: true,
		EffectiveFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if fields != nil {
		fields(&r)
	}
	return r
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

func TestPick_PriorityDesc(t *testing.T) {
	rules := []domain.FeeRule{
		mkRule(1, 10, nil),
		mkRule(2, 100, func(r *domain.FeeRule) { r.MerchantID = "mer_a" }),
		mkRule(3, 50, nil),
	}
	e := New(rules)
	in := domain.TransactionInput{MerchantID: "mer_a", AmountMinor: 1000}
	pick := e.Pick(time.Now(), in)
	if pick == nil || pick.ID != 2 {
		t.Errorf("expected rule 2 (highest priority match), got %v", pick)
	}
}

func TestPick_MerchantFallback(t *testing.T) {
	rules := []domain.FeeRule{
		mkRule(1, 10, nil), // 通配
		mkRule(2, 100, func(r *domain.FeeRule) { r.MerchantID = "mer_b" }),
	}
	e := New(rules)
	in := domain.TransactionInput{MerchantID: "mer_a", AmountMinor: 1000}
	pick := e.Pick(time.Now(), in)
	if pick == nil || pick.ID != 1 {
		t.Errorf("expected fallback rule 1, got %v", pick)
	}
}

func TestPick_AmountRange(t *testing.T) {
	rules := []domain.FeeRule{
		mkRule(1, 50, func(r *domain.FeeRule) {
			r.AmountMinMinor = 100000 // > $1000 走这条
			r.PercentBPS = 150
		}),
		mkRule(2, 10, nil), // 兜底
	}
	e := New(rules)
	// 小额
	pick := e.Pick(time.Now(), domain.TransactionInput{AmountMinor: 50000})
	if pick.ID != 2 {
		t.Errorf("small amount: expected rule 2, got %d", pick.ID)
	}
	// 大额
	pick = e.Pick(time.Now(), domain.TransactionInput{AmountMinor: 500000})
	if pick.ID != 1 {
		t.Errorf("large amount: expected rule 1, got %d", pick.ID)
	}
}

func TestPick_Inactive(t *testing.T) {
	rules := []domain.FeeRule{
		mkRule(1, 100, func(r *domain.FeeRule) { r.Active = false }),
		mkRule(2, 50, nil),
	}
	e := New(rules)
	pick := e.Pick(time.Now(), domain.TransactionInput{AmountMinor: 1000})
	if pick.ID != 2 {
		t.Errorf("inactive rule 1 should be skipped, got %d", pick.ID)
	}
}

func TestPick_EffectiveWindow(t *testing.T) {
	expired := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	rules := []domain.FeeRule{
		mkRule(1, 100, func(r *domain.FeeRule) {
			r.EffectiveFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			r.EffectiveTo = &expired
		}),
		mkRule(2, 50, nil),
	}
	e := New(rules)
	// 在窗口内
	pick := e.Pick(time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC), domain.TransactionInput{AmountMinor: 1000})
	if pick.ID != 1 {
		t.Errorf("in window: expected 1, got %d", pick.ID)
	}
	// 窗口后
	pick = e.Pick(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), domain.TransactionInput{AmountMinor: 1000})
	if pick.ID != 2 {
		t.Errorf("expired window: expected 2 fallback, got %d", pick.ID)
	}
}

func TestCompute_Basic(t *testing.T) {
	r := mkRule(1, 10, func(r *domain.FeeRule) { r.PercentBPS = 290; r.FixedMinor = 30 })
	in := domain.TransactionInput{AmountMinor: 10000, Currency: "USD"}
	fee, fx, err := Compute(&r, in, "USD")
	if err != nil {
		t.Fatal(err)
	}
	// 10000 * 0.029 + 30 = 320
	if fee != 320 {
		t.Errorf("expected 320 fee, got %d", fee)
	}
	if fx != 0 {
		t.Errorf("same currency: fx should be 0, got %d", fx)
	}
}

func TestCompute_FeeClamp(t *testing.T) {
	r := mkRule(1, 10, func(r *domain.FeeRule) {
		r.PercentBPS = 290; r.FixedMinor = 30
		r.FeeMinMinor = 100; r.FeeMaxMinor = 1000
	})
	// 小金额触发 min
	fee, _, _ := Compute(&r, domain.TransactionInput{AmountMinor: 100, Currency: "USD"}, "USD")
	if fee != 100 {
		t.Errorf("expected clamp to 100 (min), got %d", fee)
	}
	// 大金额触发 max
	fee, _, _ = Compute(&r, domain.TransactionInput{AmountMinor: 1000000, Currency: "USD"}, "USD")
	if fee != 1000 {
		t.Errorf("expected clamp to 1000 (max), got %d", fee)
	}
}

func TestCompute_FXMarkup(t *testing.T) {
	r := mkRule(1, 10, func(r *domain.FeeRule) {
		r.PercentBPS = 290; r.FixedMinor = 30
		r.FXMarkupBPS = 100 // 1%
	})
	// 同币种 — 不触发 fx
	_, fx, _ := Compute(&r, domain.TransactionInput{AmountMinor: 10000, Currency: "USD"}, "USD")
	if fx != 0 {
		t.Errorf("same currency: fx should be 0")
	}
	// 跨币种触发
	_, fx, _ = Compute(&r, domain.TransactionInput{AmountMinor: 10000, Currency: "PHP"}, "USD")
	if fx != 100 { // 10000 * 0.01
		t.Errorf("cross-currency: expected fx=100, got %d", fx)
	}
}

func TestApplyRefundPolicy(t *testing.T) {
	r := mkRule(1, 10, func(r *domain.FeeRule) { r.RefundFeeBehavior = "refund" })
	// 完全退款 → 全 fee 退
	if got := ApplyRefundPolicy(&r, 320, 10000, 10000); got != -320 {
		t.Errorf("refund full: expected -320, got %d", got)
	}
	// keep — fee 不退
	r.RefundFeeBehavior = "keep"
	if got := ApplyRefundPolicy(&r, 320, 5000, 10000); got != 0 {
		t.Errorf("keep: expected 0, got %d", got)
	}
	// prorate 50% 退
	r.RefundFeeBehavior = "prorate"
	if got := ApplyRefundPolicy(&r, 320, 5000, 10000); got != -160 {
		t.Errorf("prorate 50%%: expected -160, got %d", got)
	}
	// prorate 0% (chargeAmount=0 防 divide by zero)
	if got := ApplyRefundPolicy(&r, 320, 5000, 0); got != 0 {
		t.Errorf("prorate with zero charge: expected 0, got %d", got)
	}
}

func TestBINRange(t *testing.T) {
	if !inBINRange("400000-499999", "411111") {
		t.Errorf("411111 should match 400000-499999")
	}
	if inBINRange("400000-499999", "511111") {
		t.Errorf("511111 should not match 400000-499999")
	}
	if !inBINRange("400000-499999;510000-559999", "555555") {
		t.Errorf("555555 should match 510000-559999 (multi-range)")
	}
}

func TestCSVCurrency(t *testing.T) {
	if !inCSV("USD,PHP,SGD", "USD") {
		t.Error("USD in csv should match")
	}
	if !inCSV("USD,PHP,SGD", "php") {
		t.Error("case-insensitive should match")
	}
	if inCSV("USD,PHP,SGD", "EUR") {
		t.Error("EUR not in csv")
	}
}
