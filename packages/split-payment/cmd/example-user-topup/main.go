// example-user-topup — SP-AC-7 端到端样例.
//
// 跑:
//   go run ./cmd/example-user-topup
//
// 输出:
//   1. graph spec (从 seed JSON 反序列化)
//   2. trigger context (caller 提供的三件套 attributes)
//   3. translator 输出的 TransactionRequest 列表 (multi-leg)
//   4. 每条 leg 拆出的 借贷 entries (跟 accounting-system DoubleEntryBooking 等价)
//
// 不调 gRPC, 不写 DB — 纯 dry-run 看 translator 怎么工作.
//
// 跟 docs/SP-AC-7-EXAMPLE.md 配套.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/xiongwp/split-payment/internal/domain"
	"github.com/xiongwp/split-payment/internal/workflow"
)

func main() {
	graph := buildUserTopupGraph()

	tc := workflow.TriggerContext{
		Event:       "channel.settled",
		ChargeID:    "topup_20260513_001",
		AmountMinor: 10000,
		Currency:    "CNY",
		TraceID:     "trace_abc123",
		Attributes: map[string]string{
			"channel_receivable_account":          "PLATFORM_RECEIVABLE_CHANNEL/sub_42",
			"channel_receivable_account_amount":   "10000",
			"channel_receivable_account_currency": "CNY",

			"channel_suspense_account":          "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
			"channel_suspense_account_amount":   "10000",
			"channel_suspense_account_currency": "CNY",

			"user_id_account":          "USER_WALLET/42",
			"user_id_account_amount":   "9900",
			"user_id_account_currency": "CNY",

			"fee_clearing_account":          "PLATFORM_FEE_CLEARING/main",
			"fee_clearing_account_amount":   "100",
			"fee_clearing_account_currency": "CNY",

			"channel_fee_account":          "CHANNEL_FEE_PAYABLE/alipay",
			"channel_fee_account_amount":   "60",
			"channel_fee_account_currency": "CNY",

			"fee_account":          "PLATFORM_FEE_REVENUE/main",
			"fee_account_amount":   "40",
			"fee_account_currency": "CNY",
		},
	}

	plan, err := workflow.Translate(graph, tc)
	if err != nil {
		fmt.Println("translate error:", err)
		return
	}

	section("INPUT — Trigger Attributes")
	printJSON(tc.Attributes)

	section(fmt.Sprintf("OUTPUT — %d TransactionRequest(s)", len(plan.Transactions)))
	for i, tx := range plan.Transactions {
		fmt.Printf("\n── [%d] event_code=%q  legs=%d ──\n", i+1, tx.EventCode, len(tx.Legs))
		printJSON(tx)
	}

	section("LEDGER EQUIVALENT — 借贷 entries (per Leg)")
	totalDebit, totalCredit := int64(0), int64(0)
	for _, tx := range plan.Transactions {
		fmt.Printf("\n[event=%s]\n", tx.EventCode)
		for _, leg := range tx.Legs {
			amt, _ := strconv.ParseInt(leg.Amount, 10, 64)
			fmt.Printf("  借 %-45s %6d %s\n", leg.FromAccountID, amt, leg.Currency)
			fmt.Printf("    贷 %-43s %6d %s\n", leg.ToAccountID, amt, leg.Currency)
			totalDebit += amt
			totalCredit += amt
		}
	}
	fmt.Printf("\n借贷合计: debit=%d  credit=%d  diff=%d\n",
		totalDebit, totalCredit, totalDebit-totalCredit)

	section("Movements (兼容视图)")
	for _, m := range plan.Movements {
		fmt.Printf("  %s → %s : %d (%s)\n", m.EdgeFromNode, m.EdgeToNode, m.AmountMinor, m.Status)
	}
}

// buildUserTopupGraph 用 user_topup.graph.json 等价的 Go 结构 (避开文件 IO,聚焦数据流示意).
func buildUserTopupGraph() *domain.Graph {
	return &domain.Graph{
		ID:      1,
		Key:     "user-topup-default",
		Name:    "用户充值标准流程",
		Version: "1.0.0",
		Status:  "active",
		Spec: domain.GraphSpec{
			Scenario:       "user_topup",
			ChargeStrategy: domain.ChargeStrategySeparate,
			Triggers:       []domain.Trigger{{Event: "channel.settled"}},
			Guards: []domain.Guard{
				{Kind: "amount_min", Value: float64(1), Msg: "充值金额必须 > 0"},
			},
			Nodes: []domain.Node{
				{ID: "channel_receivable", Type: "input",
					Label:         "平台应收-渠道",
					AccountIDAttr: "channel_receivable_account"},
				{ID: "suspense", Type: "intermediate",
					Label:         "渠道入金挂账户",
					AccountIDAttr: "channel_suspense_account",
					AutoClear:     true},
				{ID: "user_wallet", Type: "account",
					Label:         "用户钱包",
					AccountIDAttr: "user_id_account"},
				{ID: "fee_clearing", Type: "intermediate",
					Label:         "平台待清算费用",
					AccountIDAttr: "fee_clearing_account",
					AutoClear:     true},
				{ID: "channel_payable", Type: "output",
					Label:         "应付渠道手续费",
					AccountIDAttr: "channel_fee_account"},
				{ID: "platform_revenue", Type: "output",
					Label:         "平台手续费收入",
					AccountIDAttr: "fee_account"},
			},
			Edges: []domain.Edge{
				{From: "channel_receivable", To: "suspense",
					Kind:      domain.EdgeKindTransfer,
					EventCode: "channel_settled_receivable",
					Rule:      domain.EdgeRule{Type: "remainder"}},
				{From: "suspense", To: "user_wallet",
					Kind:      domain.EdgeKindTransfer,
					EventCode: "channel_settled_to_user",
					Rule:      domain.EdgeRule{Type: "percent", Value: 9900}},
				{From: "suspense", To: "fee_clearing",
					Kind:      domain.EdgeKindTransfer,
					EventCode: "channel_settled_fee_pending",
					Rule:      domain.EdgeRule{Type: "remainder"}},
				{From: "fee_clearing", To: "channel_payable",
					Kind:      domain.EdgeKindTransfer,
					EventCode: "fee_cleared_to_channel_payable",
					Rule:      domain.EdgeRule{Type: "percent", Value: 6000}},
				{From: "fee_clearing", To: "platform_revenue",
					Kind:      domain.EdgeKindTransfer,
					EventCode: "fee_cleared_to_revenue",
					Rule:      domain.EdgeRule{Type: "remainder"}},
			},
		},
	}
}

func section(s string) {
	fmt.Printf("\n══════════════════════════════════════════════════════════════════\n")
	fmt.Printf(" %s\n", s)
	fmt.Printf("══════════════════════════════════════════════════════════════════\n")
}

func printJSON(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
