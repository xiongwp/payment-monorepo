package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

func mkWeightedRule(t *testing.T, ls store.LinkStore, cfg WeightedLinkFanoutConfig) engine.Rule {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := WeightedLinkFanoutFactory(ls)("wlf", "wlf", true, raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

// 10 个"现在"的 customer 边，weight ≈ 10，超 max_weight=8 → 命中
func TestWeightedLinkFanout_FreshEdges_Hits(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		ls.Link(ctx, "device:phone", customerKey(i))
	}
	r := mkWeightedRule(t, ls, WeightedLinkFanoutConfig{
		Pivot: "device", Peer: "customer",
		HalflifeDays: 30, MaxWeight: 8.0,
		Decision: "review",
	})
	hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "phone"})
	if hit == nil {
		t.Fatal("10 fresh edges should exceed max_weight=8")
	}
	if hit.Decision != engine.Review {
		t.Fatalf("expected Review, got %v", hit.Decision)
	}
}

// 仅 5 条"现在"的边，weight ≈ 5 <= 8 → 不命中
func TestWeightedLinkFanout_BelowMaxWeight_NoHit(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ls.Link(ctx, "device:phone", customerKey(i))
	}
	r := mkWeightedRule(t, ls, WeightedLinkFanoutConfig{
		Pivot: "device", Peer: "customer",
		HalflifeDays: 30, MaxWeight: 8.0,
	})
	if hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "phone"}); hit != nil {
		t.Fatalf("5 fresh edges < max_weight=8, expected no hit, got %+v", hit)
	}
}

// factory 参数校验
func TestWeightedLinkFanout_FactoryValidation(t *testing.T) {
	ls := store.NewMemLinkStore()
	bad := []WeightedLinkFanoutConfig{
		{Pivot: "", Peer: "customer", MaxWeight: 5},
		{Pivot: "device", Peer: "", MaxWeight: 5},
		{Pivot: "device", Peer: "device", MaxWeight: 5}, // same dim
		{Pivot: "device", Peer: "customer", MaxWeight: 0},
	}
	for _, c := range bad {
		raw, _ := json.Marshal(c)
		if _, err := WeightedLinkFanoutFactory(ls)("r", "n", true, raw); err == nil {
			t.Errorf("expected factory error for %+v", c)
		}
	}
}
