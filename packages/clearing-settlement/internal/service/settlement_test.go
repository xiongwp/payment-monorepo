package service

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestTriggerSettlement_RunIDIncrements(t *testing.T) {
	svc := NewSettlementService(zap.NewNop())
	ctx := context.Background()

	r1, err := svc.TriggerSettlement(ctx, "2026-04-28", "PHP")
	if err != nil {
		t.Fatalf("trigger 1 err: %v", err)
	}
	if r1 != 1 {
		t.Fatalf("expected run_id=1, got %d", r1)
	}

	r2, err := svc.TriggerSettlement(ctx, "2026-04-28", "PHP")
	if err != nil {
		t.Fatalf("trigger 2 err: %v", err)
	}
	if r2 != 2 {
		t.Fatalf("expected run_id=2 (rerun), got %d", r2)
	}

	// 不同 currency 各自独立计数
	rUSD, err := svc.TriggerSettlement(ctx, "2026-04-28", "USD")
	if err != nil {
		t.Fatalf("trigger USD err: %v", err)
	}
	if rUSD != 1 {
		t.Fatalf("expected USD run_id=1, got %d", rUSD)
	}
}

func TestTriggerSettlement_RejectsEmptyArgs(t *testing.T) {
	svc := NewSettlementService(zap.NewNop())
	if _, err := svc.TriggerSettlement(context.Background(), "", "PHP"); err == nil {
		t.Fatal("expected error for empty settle_date")
	}
	if _, err := svc.TriggerSettlement(context.Background(), "2026-04-28", ""); err == nil {
		t.Fatal("expected error for empty currency")
	}
}
