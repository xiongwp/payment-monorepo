// translator_test.go — SP-AC-7: multi-leg per rule (event_code 分组).
package workflow

import (
	"strings"
	"testing"

	"github.com/xiongwp/split-payment/internal/domain"
)

func mkAccountingGraph(nodes []domain.Node, edges []domain.Edge) *domain.Graph {
	return &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Scenario: "user_topup",
			Nodes:    nodes,
			Edges:    edges,
		},
	}
}

func parseInt(s string) int64 {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// TestTranslate_MultiLegSameEventCode 一个 event_code 包含 3 条 edge → 一个 TransactionRequest 带 3 个 leg.
func TestTranslate_MultiLegSameEventCode(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "channel_recv", Type: "input"},
			{ID: "suspense", Type: "intermediate"},
			{ID: "user", Type: "account", AccountIDAttr: "user_id_account"},
			{ID: "fee_clearing", Type: "intermediate"},
		},
		[]domain.Edge{
			// Phase 1: channel.settled 包含 3 条 edge 共享同一 event_code
			{From: "channel_recv", To: "suspense", EventCode: "channel_settled",
				Rule: domain.EdgeRule{Type: "remainder"}},
			{From: "suspense", To: "user", EventCode: "channel_settled",
				Rule: domain.EdgeRule{Type: "percent", Value: 9900}},
			{From: "suspense", To: "fee_clearing", EventCode: "channel_settled",
				Rule: domain.EdgeRule{Type: "remainder"}},
		},
	)
	tc := TriggerContext{
		ChargeID: "topup_1", Currency: "CNY", AmountMinor: 10000,
		Attributes: map[string]string{
			"user_id_account":          "42",
			"user_id_account_amount":   "9900",
			"user_id_account_currency": "CNY",
		},
	}
	plan, err := Translate(g, tc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Transactions) != 1 {
		t.Fatalf("want 1 transaction (one rule group) got %d", len(plan.Transactions))
	}
	tx := plan.Transactions[0]
	if tx.EventCode != "channel_settled" {
		t.Errorf("event=%q want channel_settled", tx.EventCode)
	}
	if len(tx.Legs) != 3 {
		t.Errorf("want 3 legs got %d", len(tx.Legs))
	}
}

// TestTranslate_MultipleRuleGroups 两个 event_code 各包含若干 edge → 两个 TransactionRequest.
func TestTranslate_MultipleRuleGroups(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "src", Type: "input"},
			{ID: "user", Type: "account"},
			{ID: "fee", Type: "intermediate"},
			{ID: "channel_payable", Type: "output"},
			{ID: "platform_revenue", Type: "output"},
		},
		[]domain.Edge{
			// rule 1: channel_settled (2 legs)
			{From: "src", To: "user", EventCode: "channel_settled",
				Rule: domain.EdgeRule{Type: "percent", Value: 9900}},
			{From: "src", To: "fee", EventCode: "channel_settled",
				Rule: domain.EdgeRule{Type: "remainder"}},
			// rule 2: fee_cleared (2 legs)
			{From: "fee", To: "channel_payable", EventCode: "fee_cleared",
				Rule: domain.EdgeRule{Type: "percent", Value: 6000}},
			{From: "fee", To: "platform_revenue", EventCode: "fee_cleared",
				Rule: domain.EdgeRule{Type: "remainder"}},
		},
	)
	plan, err := Translate(g, TriggerContext{
		ChargeID: "topup_2", Currency: "CNY", AmountMinor: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Transactions) != 2 {
		t.Fatalf("want 2 transactions (2 rule groups) got %d", len(plan.Transactions))
	}
	// 第一个 group: channel_settled
	if plan.Transactions[0].EventCode != "channel_settled" || len(plan.Transactions[0].Legs) != 2 {
		t.Errorf("group 0 wrong: %+v", plan.Transactions[0])
	}
	if plan.Transactions[1].EventCode != "fee_cleared" || len(plan.Transactions[1].Legs) != 2 {
		t.Errorf("group 1 wrong: %+v", plan.Transactions[1])
	}
}

// TestTranslate_EventDrivenAmount to-node 配 attr 三件套, 直接透传金额.
func TestTranslate_EventDrivenAmount(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "src", Type: "input", AccountIDAttr: "channel_account"},
			{ID: "user", Type: "account", AccountIDAttr: "user_id_account"},
		},
		[]domain.Edge{
			{From: "src", To: "user", EventCode: "channel_settled_to_user",
				Rule: domain.EdgeRule{Type: "remainder"}},
		},
	)
	tc := TriggerContext{
		ChargeID: "topup_3", Currency: "CNY", AmountMinor: 10000,
		Attributes: map[string]string{
			"channel_account":          "ch_x",
			"user_id_account":          "42",
			"user_id_account_amount":   "9900",
			"user_id_account_currency": "CNY",
		},
	}
	plan, err := Translate(g, tc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Transactions) != 1 || len(plan.Transactions[0].Legs) != 1 {
		t.Fatalf("want 1 tx 1 leg got %+v", plan.Transactions)
	}
	leg := plan.Transactions[0].Legs[0]
	if leg.Amount != "9900" || leg.Currency != "CNY" {
		t.Errorf("amount=%s currency=%s want 9900/CNY", leg.Amount, leg.Currency)
	}
	if leg.FromAccountID != "ch_x" || leg.ToAccountID != "42" {
		t.Errorf("from=%s to=%s want ch_x→42", leg.FromAccountID, leg.ToAccountID)
	}
}

// TestTranslate_TailRoundingGoesToLast 33%+33%+34% remainder 尾差归本组最后 leg.
func TestTranslate_TailRoundingGoesToLast(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "src", Type: "input"},
			{ID: "a", Type: "account"},
			{ID: "b", Type: "account"},
			{ID: "c", Type: "account"},
		},
		[]domain.Edge{
			{From: "src", To: "a", EventCode: "split",
				Rule: domain.EdgeRule{Type: "percent", Value: 3333}},
			{From: "src", To: "b", EventCode: "split",
				Rule: domain.EdgeRule{Type: "percent", Value: 3333}},
			{From: "src", To: "c", EventCode: "split",
				Rule: domain.EdgeRule{Type: "percent", Value: 3334}},
		},
	)
	plan, err := Translate(g, TriggerContext{AmountMinor: 100, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	sum := int64(0)
	for _, leg := range plan.Transactions[0].Legs {
		sum += parseInt(leg.Amount)
	}
	if sum != 100 {
		t.Errorf("sum=%d want 100 (尾差丢了)", sum)
	}
}

// TestTranslate_MixedCurrencyInGroup 同一 event_code 组内混币 → 报错.
func TestTranslate_MixedCurrencyInGroup(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "src", Type: "input"},
			{ID: "a", Type: "account", AccountIDAttr: "a_acct"},
			{ID: "b", Type: "account", AccountIDAttr: "b_acct"},
		},
		[]domain.Edge{
			{From: "src", To: "a", EventCode: "split",
				Rule: domain.EdgeRule{Type: "percent", Value: 5000}},
			{From: "src", To: "b", EventCode: "split",
				Rule: domain.EdgeRule{Type: "remainder"}},
		},
	)
	tc := TriggerContext{
		AmountMinor: 100, Currency: "USD",
		Attributes: map[string]string{
			"a_acct": "X", "a_acct_amount": "50", "a_acct_currency": "USD",
			"b_acct": "Y", "b_acct_amount": "50", "b_acct_currency": "EUR", // 混币
		},
	}
	if _, err := Translate(g, tc); err == nil {
		t.Fatal("expected mixed-currency error")
	} else if !strings.Contains(err.Error(), "currenc") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestTranslate_GuardAmountMin guard 校验.
func TestTranslate_GuardAmountMin(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Scenario: "user_topup",
			Guards:   []domain.Guard{{Kind: "amount_min", Value: float64(100), Msg: "too small"}},
			Nodes:    []domain.Node{{ID: "src", Type: "input"}},
		},
	}
	if _, err := Translate(g, TriggerContext{AmountMinor: 50}); err == nil {
		t.Fatal("expected guard fail")
	}
	if _, err := Translate(g, TriggerContext{AmountMinor: 200}); err != nil {
		t.Fatalf("guard should pass: %v", err)
	}
}

// TestTranslate_MissingScenario 没配 scenario → 报错.
func TestTranslate_MissingScenario(t *testing.T) {
	g := &domain.Graph{
		ID: 1, Version: "1.0.0",
		Spec: domain.GraphSpec{
			Nodes: []domain.Node{{ID: "src", Type: "input"}},
		},
	}
	if _, err := Translate(g, TriggerContext{AmountMinor: 100}); err == nil {
		t.Fatal("expected error for missing scenario")
	}
}

// TestTranslate_MissingEventCode edge 没 event_code → 报错.
func TestTranslate_MissingEventCode(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "src", Type: "input"},
			{ID: "user", Type: "account"},
		},
		[]domain.Edge{
			{From: "src", To: "user", Rule: domain.EdgeRule{Type: "remainder"}},
		},
	)
	if _, err := Translate(g, TriggerContext{AmountMinor: 100, Currency: "USD"}); err == nil {
		t.Fatal("expected error for missing event_code")
	}
}

// TestTranslate_MissingAccountAttr 节点配了 AccountIDAttr 但 attributes 里没值 → 报错.
func TestTranslate_MissingAccountAttr(t *testing.T) {
	g := mkAccountingGraph(
		[]domain.Node{
			{ID: "src", Type: "input"},
			{ID: "user", Type: "account", AccountIDAttr: "user_id_account"},
		},
		[]domain.Edge{
			{From: "src", To: "user", EventCode: "ev",
				Rule: domain.EdgeRule{Type: "remainder"}},
		},
	)
	if _, err := Translate(g, TriggerContext{AmountMinor: 100, Currency: "USD"}); err == nil {
		t.Fatal("expected error for missing user_id_account attr")
	}
}
