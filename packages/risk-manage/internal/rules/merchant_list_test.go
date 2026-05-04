package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/merchantlist"
)

func newRule(t *testing.T, factory func(merchantlist.Service) engine.RuleFactory, svc merchantlist.Service) engine.Rule {
	t.Helper()
	r, err := factory(svc)("m_l", "merchant list", true, json.RawMessage("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMerchantAllowlist_HitsCustomer(t *testing.T) {
	svc := merchantlist.NewMemService()
	_ = svc.Add(context.Background(), merchantlist.Entry{
		MerchantID: "m1", Kind: merchantlist.KindAllow, Dimension: "customer", Value: "vip1",
	})
	r := newRule(t, MerchantAllowlistFactory, svc)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		MerchantID: "m1", CustomerID: "vip1",
	})
	if h == nil {
		t.Fatal("expected hit")
	}
	if !h.Force || h.Decision != engine.Allow {
		t.Fatalf("expected Force allow, got %+v", h)
	}
}

func TestMerchantBlocklist_HitsIP(t *testing.T) {
	svc := merchantlist.NewMemService()
	_ = svc.Add(context.Background(), merchantlist.Entry{
		MerchantID: "m1", Kind: merchantlist.KindBlock, Dimension: "ip", Value: "1.2.3.4", Reason: "chargeback ring",
	})
	r := newRule(t, MerchantBlocklistFactory, svc)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		MerchantID: "m1", IPAddress: "1.2.3.4",
	})
	if h == nil {
		t.Fatal("expected hit")
	}
	if !h.Force || h.Decision != engine.Deny {
		t.Fatalf("expected Force deny, got %+v", h)
	}
}

func TestMerchantList_NoMerchant_NoHit(t *testing.T) {
	svc := merchantlist.NewMemService()
	r := newRule(t, MerchantAllowlistFactory, svc)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{CustomerID: "any"}); h != nil {
		t.Fatalf("no merchant_id should not hit, got %+v", h)
	}
}

func TestMerchantList_Metadata_CardFingerprint(t *testing.T) {
	svc := merchantlist.NewMemService()
	_ = svc.Add(context.Background(), merchantlist.Entry{
		MerchantID: "m1", Kind: merchantlist.KindBlock, Dimension: "card_fingerprint", Value: "fp_abc",
	})
	r := newRule(t, MerchantBlocklistFactory, svc)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		MerchantID: "m1",
		Metadata:   map[string]string{"card_fingerprint": "fp_abc"},
	})
	if h == nil || !h.Force || h.Decision != engine.Deny {
		t.Fatalf("card_fingerprint block should hit force-deny, got %+v", h)
	}
}

func TestMerchantList_KindIsolation(t *testing.T) {
	svc := merchantlist.NewMemService()
	_ = svc.Add(context.Background(), merchantlist.Entry{
		MerchantID: "m1", Kind: merchantlist.KindAllow, Dimension: "customer", Value: "vip1",
	})
	// 只有 allow 名单，但调 blocklist 规则 → 不命中
	r := newRule(t, MerchantBlocklistFactory, svc)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{MerchantID: "m1", CustomerID: "vip1"}); h != nil {
		t.Fatalf("blocklist rule should not match allow entry, got %+v", h)
	}
}
