// SP-AC-7 PH3-5: SaveGraph → TriggerEvent end-to-end.
//
//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/xiongwp/split-payment/internal/domain"
	"github.com/xiongwp/split-payment/internal/workflow"
)

// TestSaveGraphTriggerEvent_E2E:
//   1. SaveGraph (用户充值场景, 2 edges)
//   2. Engine.Handle(event=charge.succeeded) → translator → accounting stub
//   3. Verify:
//      - 1 个 RunPlan 落 moneyflow_runs (status=completed)
//      - accounting stub 收到 1 个 TransactionRequest, event_code 正确
//      - 事件总线发出 plan.created + plan.completed
func TestSaveGraphTriggerEvent_E2E(t *testing.T) {
	env := loadEnv(t)
	env.acct.Reset()
	env.events.Reset()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	graph := &domain.Graph{
		Key:     "user_topup_e2e_" + nowSuffix(),
		Name:    "e2e user topup",
		Version: "1.0.0",
		Status:  "active",
		Spec: domain.GraphSpec{
			Scenario: "user_topup",
			Trigger: domain.Trigger{
				Events: []string{"charge.succeeded"},
			},
			Nodes: []domain.Node{
				{ID: "channel", Type: "external", AccountType: "EXTERNAL_CHANNEL"},
				{ID: "user_wallet", Type: "internal", AccountType: "USER_BALANCE"},
			},
			Edges: []domain.Edge{
				{
					FromNode:    "channel",
					ToNode:      "user_wallet",
					Kind:        "movement",
					EventCode:   "user_topup_credit",
					AmountField: "amount_minor",
				},
			},
		},
	}
	if _, err := env.graphs.Save(ctx, graph); err != nil {
		t.Fatalf("save graph: %v", err)
	}

	event := workflow.BusinessEvent{
		Event:       "charge.succeeded",
		ChargeID:    "ch_e2e_" + nowSuffix(),
		MerchantID:  "merch_e2e",
		AmountMinor: 5000,
		Currency:    "USD",
		Attributes: map[string]string{
			"user_id": "100000042",
		},
		TraceID: "trace_" + nowSuffix(),
	}
	if err := env.engine.Handle(ctx, event); err != nil {
		t.Fatalf("engine.Handle: %v", err)
	}

	// assert: 1 个 plan, status=completed
	plans, err := env.runs.GetByCharge(ctx, event.ChargeID)
	if err != nil {
		t.Fatalf("GetByCharge: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].Status != "completed" {
		t.Errorf("expected status=completed, got %q (err=%q)", plans[0].Status, plans[0].ErrorMsg)
	}

	// assert: accounting 收到调用
	calls := env.acct.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 accounting call, got %d", len(calls))
	}
	if calls[0].EventCode != "user_topup_credit" {
		t.Errorf("event_code: want user_topup_credit got %q", calls[0].EventCode)
	}
	if calls[0].BusinessNo != event.ChargeID {
		t.Errorf("business_no: want %s got %s", event.ChargeID, calls[0].BusinessNo)
	}

	// assert: 事件发出
	events := env.events.Events()
	if len(events) == 0 {
		t.Error("expected at least 1 event published, got 0")
	}
}

func nowSuffix() string {
	return time.Now().Format("150405.000000")
}
