package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

func mkLinkRule(t *testing.T, ls store.LinkStore, cfg LinkFanoutConfig) engine.Rule {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := LinkFanoutFactory(ls)("rid", "rule-name", true, raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

func TestLinkFanout_BelowThresholdNoHit(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ls.Link(ctx, "device:phone", customerKey(i))
	}
	r := mkLinkRule(t, ls, LinkFanoutConfig{Pivot: "device", Peer: "customer", Threshold: 5})
	hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "phone"})
	if hit != nil {
		t.Fatalf("3 < threshold 5; expected no hit, got %+v", hit)
	}
}

func TestLinkFanout_ExceedsThresholdHits(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		ls.Link(ctx, "device:phone", customerKey(i))
	}
	r := mkLinkRule(t, ls, LinkFanoutConfig{Pivot: "device", Peer: "customer", Threshold: 5, Decision: "review"})
	hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "phone"})
	if hit == nil {
		t.Fatal("6 > threshold 5; expected hit")
	}
	if hit.Decision != engine.Review {
		t.Fatalf("expected Review verdict, got %v", hit.Decision)
	}
}

func TestLinkFanout_DenyDecision(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		ls.Link(ctx, "ip:1.2.3.4", customerKey(i))
	}
	r := mkLinkRule(t, ls, LinkFanoutConfig{Pivot: "ip", Peer: "customer", Threshold: 3, Decision: "deny"})
	hit := r.Evaluate(ctx, &engine.TxnContext{IPAddress: "1.2.3.4"})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected Deny, got %+v", hit)
	}
}

func TestLinkFanout_PivotMissingFromTxn_NoHit(t *testing.T) {
	ls := store.NewMemLinkStore()
	r := mkLinkRule(t, ls, LinkFanoutConfig{Pivot: "device", Peer: "customer", Threshold: 1})
	// txn 没填 device → 不命中（保守 fail-open）
	hit := r.Evaluate(context.Background(), &engine.TxnContext{CustomerID: "c1"})
	if hit != nil {
		t.Fatalf("expected nil hit when pivot missing, got %+v", hit)
	}
}

func TestLinkFanout_PrefixIsolatesPeerDimension(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	// 5 个 customer + 5 个 ip 都关联 device:phone
	for i := 0; i < 5; i++ {
		ls.Link(ctx, "device:phone", customerKey(i))
		ls.Link(ctx, "device:phone", ipKey(i))
	}
	// 规则只看 customer 维度，threshold=4 → customer 数 5 > 4，命中
	r := mkLinkRule(t, ls, LinkFanoutConfig{Pivot: "device", Peer: "customer", Threshold: 4})
	if hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "phone"}); hit == nil {
		t.Fatal("customer count 5 > 4: expected hit")
	}
	// threshold=5 → customer 数 5 == 5，不命中（严格 >）
	r2 := mkLinkRule(t, ls, LinkFanoutConfig{Pivot: "device", Peer: "customer", Threshold: 5})
	if hit := r2.Evaluate(ctx, &engine.TxnContext{DeviceID: "phone"}); hit != nil {
		t.Fatalf("strict > expected nil at equal threshold, got %+v", hit)
	}
}

func TestLinkFanout_FactoryRejectsBadConfig(t *testing.T) {
	ls := store.NewMemLinkStore()
	bad := []LinkFanoutConfig{
		{Pivot: "", Peer: "customer", Threshold: 5},
		{Pivot: "device", Peer: "", Threshold: 5},
		{Pivot: "device", Peer: "device", Threshold: 5},
		{Pivot: "device", Peer: "customer", Threshold: 0},
	}
	for _, cfg := range bad {
		raw, _ := json.Marshal(cfg)
		if _, err := LinkFanoutFactory(ls)("r", "n", true, raw); err == nil {
			t.Errorf("expected factory error for cfg %+v", cfg)
		}
	}
}

// helpers
func customerKey(i int) string { return "customer:" + itoa(i) }
func ipKey(i int) string       { return "ip:" + itoa(i) }
func itoa(i int) string {
	// 简化版 strconv.Itoa；测试用，只接受 0-99
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+(i/10))) + string(rune('0'+(i%10)))
}
