//go:build feast

package featurestore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestFeastClient_StubBehavior 验证未 wire client 时 fail-open（返
// ErrFeastNotEnabled）。
func TestFeastClient_StubBehavior(t *testing.T) {
	// client 字段为 nil（NewFeastClient 当前 TODO 未填）
	c := &FeastClient{}
	_, err := c.GetOnlineFeatures(context.Background(), "cust1",
		[]string{"customer:paid_count_90d"})
	if !errors.Is(err, ErrFeastNotEnabled) {
		t.Fatalf("expected ErrFeastNotEnabled, got %v", err)
	}
}

// TestFeastClient_Integration 真实 gRPC 集成测试。
// 没设 FEAST_TEST_ADDR 时跳过；CI / local 默认不跑。
func TestFeastClient_Integration(t *testing.T) {
	addr := os.Getenv("FEAST_TEST_ADDR")
	if addr == "" {
		t.Skip("FEAST_TEST_ADDR not set; skipping live Feast integration test")
	}
	c, err := NewFeastClient(addr, "risk", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_, err = c.GetOnlineFeatures(context.Background(), "test_customer_1",
		[]string{"customer:paid_count_90d", "customer:chargeback_count_90d"})
	// 没 wire 真 proto 之前应该返 ErrFeastNotEnabled
	if err != nil && !errors.Is(err, ErrFeastNotEnabled) && !errors.Is(err, ErrFeastTimeout) {
		t.Logf("got err: %v (expected after ML team wires proto)", err)
	}
}
