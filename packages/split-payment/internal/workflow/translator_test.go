package workflow

import (
	"testing"

	"reconcile-system/packages/split-payment/internal/domain"
)

func mkGraph(items ...domain.Node) *domain.Graph {
	return &domain.Graph{ID: 1, Version: "1.0.0", Spec: domain.GraphSpec{Nodes: items}}
}

func TestTranslate_BasicPercentSplit(t *testing.T) {
	// 90/5/5 marketplace 分账
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Nodes: []domain.Node{
				{ID: "src",      Type: "input",   AccountTemplate: "platform_collected/{merchant_id}"},
				{ID: "platform", Type: "account", AccountTemplate: "platform_fee/{merchant_id}"},
				{ID: "seller",   Type: "account", AccountTemplate: "seller_balance/{seller_id}", FromAttr: "seller_id"},
			},
			Edges: []domain.Edge{
				{From: "src", To: "platform", Rule: domain.EdgeRule{Type: "percent", Value: 500}}, // 5%
				{From: "src", To: "seller",   Rule: domain.EdgeRule{Type: "remainder"}},
			},
		},
	}
	tc := TriggerContext{
		ChargeID: "ch_1", MerchantID: "mer_a", AmountMinor: 10000, Currency: "USD",
		Attributes: map[string]string{"seller_id": "s1"},
	}
	plan, err := Translate(g, tc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Movements) != 2 {
		t.Fatalf("want 2 movements, got %d", len(plan.Movements))
	}
	// 平台 500, 卖家 9500
	sum := int64(0)
	for _, m := range plan.Movements {
		sum += m.AmountMinor
	}
	if sum != 10000 {
		t.Errorf("sum=%d want 10000 (lost money!)", sum)
	}
}

func TestTranslate_TailRoundingGoesToLast(t *testing.T) {
	// 33% × 3 = 99%, 余 1% 应归最后一条 movement (防丢钱)
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Nodes: []domain.Node{
				{ID: "src", Type: "input", AccountTemplate: "src"},
				{ID: "a",   Type: "account", AccountTemplate: "a"},
				{ID: "b",   Type: "account", AccountTemplate: "b"},
				{ID: "c",   Type: "account", AccountTemplate: "c"},
			},
			Edges: []domain.Edge{
				{From: "src", To: "a", Rule: domain.EdgeRule{Type: "percent", Value: 3333}},
				{From: "src", To: "b", Rule: domain.EdgeRule{Type: "percent", Value: 3333}},
				{From: "src", To: "c", Rule: domain.EdgeRule{Type: "percent", Value: 3334}},
			},
		},
	}
	tc := TriggerContext{AmountMinor: 100}
	plan, err := Translate(g, tc)
	if err != nil {
		t.Fatal(err)
	}
	sum := int64(0)
	for _, m := range plan.Movements {
		sum += m.AmountMinor
	}
	if sum != 100 {
		t.Errorf("sum=%d want 100 (尾差丢了)", sum)
	}
}

func TestTranslate_PlaceholderMissing_OptionalSkip(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Nodes: []domain.Node{
				{ID: "src",      Type: "input",   AccountTemplate: "src"},
				{ID: "platform", Type: "account", AccountTemplate: "platform"},
				{ID: "ref",      Type: "account", AccountTemplate: "referrer/{referrer}", FromAttr: "referrer", Optional: true},
				{ID: "seller",   Type: "account", AccountTemplate: "seller"},
			},
			Edges: []domain.Edge{
				{From: "src", To: "platform", Rule: domain.EdgeRule{Type: "percent", Value: 500}},
				{From: "src", To: "ref",      Rule: domain.EdgeRule{Type: "percent", Value: 500, IfMissing: "skip"}},
				{From: "src", To: "seller",   Rule: domain.EdgeRule{Type: "remainder"}},
			},
		},
	}
	// 不传 referrer → ref 节点 skipped, edge skipped, 平台 500 + 卖家 9500
	tc := TriggerContext{AmountMinor: 10000}
	plan, err := Translate(g, tc)
	if err != nil {
		t.Fatal(err)
	}
	sum := int64(0)
	for _, m := range plan.Movements {
		if m.Status == "pending" {
			sum += m.AmountMinor
		}
	}
	if sum != 10000 {
		t.Errorf("optional skipped 后 sum=%d want 10000", sum)
	}
}

func TestTranslate_PlaceholderMissing_RequiredFails(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Nodes: []domain.Node{
				{ID: "src",    Type: "input",   AccountTemplate: "src"},
				{ID: "seller", Type: "account", AccountTemplate: "seller/{seller_id}", FromAttr: "seller_id"},
			},
			Edges: []domain.Edge{
				{From: "src", To: "seller", Rule: domain.EdgeRule{Type: "remainder"}},
			},
		},
	}
	tc := TriggerContext{AmountMinor: 1000} // 没 seller_id, 节点非 optional → 报错
	_, err := Translate(g, tc)
	if err == nil {
		t.Fatal("expected error for missing required attribute")
	}
}

func TestTranslate_GuardAmountMin(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Guards: []domain.Guard{{Kind: "amount_min", Value: float64(100), Msg: "too small"}},
			Nodes:  []domain.Node{{ID: "src", Type: "input", AccountTemplate: "s"}},
			Edges:  []domain.Edge{},
		},
	}
	_, err := Translate(g, TriggerContext{AmountMinor: 50})
	if err == nil {
		t.Fatal("expected guard fail")
	}
	_, err = Translate(g, TriggerContext{AmountMinor: 200})
	if err != nil {
		t.Fatalf("guard should pass: %v", err)
	}
}

func TestTranslate_CurrencyGuard(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Guards: []domain.Guard{{Kind: "currency_in", Value: []any{"USD", "EUR"}, Msg: "bad currency"}},
			Nodes:  []domain.Node{{ID: "src", Type: "input", AccountTemplate: "s"}},
		},
	}
	_, err := Translate(g, TriggerContext{AmountMinor: 100, Currency: "JPY"})
	if err == nil {
		t.Fatal("expected JPY rejected")
	}
}

func TestTranslate_FixedThenRemainder(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Nodes: []domain.Node{
				{ID: "src",    Type: "input",   AccountTemplate: "src"},
				{ID: "tax",    Type: "account", AccountTemplate: "tax"},
				{ID: "seller", Type: "account", AccountTemplate: "seller"},
			},
			Edges: []domain.Edge{
				{From: "src", To: "tax",    Rule: domain.EdgeRule{Type: "fixed_minor", Value: 100}}, // $1 税
				{From: "src", To: "seller", Rule: domain.EdgeRule{Type: "remainder"}},
			},
		},
	}
	plan, err := Translate(g, TriggerContext{AmountMinor: 1000})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, m := range plan.Movements {
		got[m.ToAccount] = m.AmountMinor
	}
	if got["tax"] != 100 {
		t.Errorf("tax=%d want 100", got["tax"])
	}
	if got["seller"] != 900 {
		t.Errorf("seller=%d want 900", got["seller"])
	}
}
