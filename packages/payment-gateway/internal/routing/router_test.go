// router_test.go — 路由 filter / score / Decide 测试。

package routing

import (
	"testing"
	"time"
)

func sampleChannels() []Channel {
	return []Channel{
		{ID: "visa-stripe", Products: []string{"card_charge"},
			SupportedBINs: []string{"400000-499999"},
			SupportedCcy:  []string{"USD", "PHP"}, Regions: []string{"PH", "US"},
			FeeBPS: 290, Priority: 80},
		{ID: "mc-adyen", Products: []string{"card_charge"},
			SupportedBINs: []string{"510000-559999"},
			SupportedCcy:  []string{"USD", "PHP"}, Regions: []string{"PH"},
			FeeBPS: 270, Priority: 70},
		{ID: "gcash-direct", Products: []string{"wallet"},
			SupportedCcy: []string{"PHP"}, Regions: []string{"PH"},
			FeeBPS: 150, Priority: 90},
	}
}

func TestFilter_ProductMismatch(t *testing.T) {
	r := New(sampleChannels())
	dec, err := r.Decide(Request{Product: "wallet", Currency: "PHP", Region: "PH", AmountMinor: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Primary.ID != "gcash-direct" {
		t.Errorf("expected gcash-direct, got %s", dec.Primary.ID)
	}
}

func TestFilter_BIN(t *testing.T) {
	r := New(sampleChannels())
	// Visa BIN
	dec, _ := r.Decide(Request{Product: "card_charge", Currency: "PHP",
		Region: "PH", CardBIN: "411111", AmountMinor: 1000})
	if dec.Primary.ID != "visa-stripe" {
		t.Errorf("Visa BIN: expected visa-stripe, got %s", dec.Primary.ID)
	}
	// MC BIN
	dec, _ = r.Decide(Request{Product: "card_charge", Currency: "PHP",
		Region: "PH", CardBIN: "555555", AmountMinor: 1000})
	if dec.Primary.ID != "mc-adyen" {
		t.Errorf("MC BIN: expected mc-adyen, got %s", dec.Primary.ID)
	}
}

func TestFilter_NoMatch(t *testing.T) {
	r := New(sampleChannels())
	// 不支持的 region
	_, err := r.Decide(Request{Product: "card_charge", Currency: "EUR",
		Region: "DE", CardBIN: "411111", AmountMinor: 1000})
	if err == nil {
		t.Errorf("expected error for unsupported region/currency")
	}
}

func TestScore_HealthDegrades(t *testing.T) {
	r := New(sampleChannels())
	// 反复让 visa-stripe 失败 — 它的 score 应该降到比 mc-adyen 还低
	for i := 0; i < 50; i++ {
		r.UpdateHealth("visa-stripe", false, 5*time.Second)
	}
	for i := 0; i < 50; i++ {
		r.UpdateHealth("mc-adyen", true, 50*time.Millisecond)
	}
	// 此时同样的请求应该优先走 mc-adyen
	dec, _ := r.Decide(Request{Product: "card_charge", Currency: "PHP",
		Region: "PH", AmountMinor: 1000, CardBIN: ""})
	// 没 BIN 时两家都符合，按 health 排
	if dec.Primary.ID != "mc-adyen" {
		t.Errorf("after visa degraded, expected mc-adyen primary, got %s", dec.Primary.ID)
	}
}

func TestScore_MerchantPref(t *testing.T) {
	r := New(sampleChannels())
	dec, _ := r.Decide(Request{
		Product: "card_charge", Currency: "PHP", Region: "PH", AmountMinor: 1000,
		MerchantPrefs: []string{"mc-adyen"},
	})
	if dec.Primary.ID != "mc-adyen" {
		t.Errorf("merchant prefers mc-adyen, expected primary mc-adyen, got %s", dec.Primary.ID)
	}
}

func TestDecide_FallbackList(t *testing.T) {
	r := New(sampleChannels())
	dec, err := r.Decide(Request{
		Product: "card_charge", Currency: "PHP", Region: "PH", AmountMinor: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 至少有 primary，fallback 可能 1 个
	if dec.Primary == nil {
		t.Fatal("primary nil")
	}
	if len(dec.Reasoning) == 0 {
		t.Error("expected reasoning")
	}
	if len(dec.Scores) < 2 {
		t.Error("expected at least 2 scored channels")
	}
}
