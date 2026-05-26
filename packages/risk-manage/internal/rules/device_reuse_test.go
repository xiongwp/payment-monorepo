package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func mkDeviceReuseRule(t *testing.T, cfg DeviceReuseConfig) engine.Rule {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := DeviceReuseFactory()("rid", "rule-name", true, raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

func TestDeviceReuse_BelowThresholdNoHit(t *testing.T) {
	r := mkDeviceReuseRule(t, DeviceReuseConfig{Threshold: 5})
	hit := r.Evaluate(context.Background(),
		&engine.TxnContext{DeviceDistinctCustomers90d: 3})
	if hit != nil {
		t.Fatalf("3 <= 5 should not hit, got %+v", hit)
	}
}

func TestDeviceReuse_AboveThresholdReview(t *testing.T) {
	r := mkDeviceReuseRule(t, DeviceReuseConfig{Threshold: 5, Decision: "review"})
	hit := r.Evaluate(context.Background(),
		&engine.TxnContext{DeviceDistinctCustomers90d: 8})
	if hit == nil {
		t.Fatal("expected hit when 8 > 5")
	}
	if hit.Decision != engine.Review {
		t.Fatalf("expected Review, got %v", hit.Decision)
	}
}

func TestDeviceReuse_DenyVerdict(t *testing.T) {
	r := mkDeviceReuseRule(t, DeviceReuseConfig{Threshold: 3, Decision: "deny"})
	hit := r.Evaluate(context.Background(),
		&engine.TxnContext{DeviceDistinctCustomers90d: 12})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected Deny, got %+v", hit)
	}
}

func TestDeviceReuse_CustomerDimensionReverse(t *testing.T) {
	r := mkDeviceReuseRule(t, DeviceReuseConfig{
		Dimension: "customer_devices",
		Threshold: 3,
		Decision:  "review",
	})
	// customer 用了 5 个不同设备 → 命中
	hit := r.Evaluate(context.Background(),
		&engine.TxnContext{CustomerDistinctDevices90d: 5})
	if hit == nil {
		t.Fatal("customer_devices 5 > 3 should hit")
	}
}

func TestDeviceReuse_FactoryValidatesConfig(t *testing.T) {
	bad := []DeviceReuseConfig{
		{Threshold: 0},
		{Dimension: "wat", Threshold: 5},
	}
	for _, c := range bad {
		raw, _ := json.Marshal(c)
		if _, err := DeviceReuseFactory()("r", "n", true, raw); err == nil {
			t.Errorf("expected factory error for cfg %+v", c)
		}
	}
}
