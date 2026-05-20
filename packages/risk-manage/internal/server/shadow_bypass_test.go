// shadow 短路单测：验证压测流量在 Screen / BulkScreen / Report 入口直接放行，
// 不调底层 svc / store / 外部反欺诈。
package server

import (
	"context"
	"testing"

	"go.uber.org/zap"

	riskv1 "github.com/xiongwp/risk-manage/kitex_gen/risk/v1"
	"github.com/xiongwp/payment-util/shadow"
)

// 构造一个 svc/store 全部 nil 的 Server——shadow 短路必须在这些被 deref 之前生效，
// 所以 nil 都不会 panic 才说明短路有效。
func newShadowOnlyServer() *Server {
	return &Server{
		svc:     nil, // shadow 流量绝不调用 → nil 安全
		bl:      nil,
		auth:    nil,
		apiKeys: nil,
		limiter: nil,
		logger:  zap.NewNop(),
	}
}

func TestScreen_ShadowBypass(t *testing.T) {
	s := newShadowOnlyServer()
	ctx := shadow.WithShadow(context.Background(), true)
	resp, err := s.Screen(ctx, &riskv1.ScreenRequest{
		MerchantId:      "any-merchant",
		PaymentIntentId: "pi_load_test",
		Amount:          999999,
	})
	if err != nil {
		t.Fatalf("Screen returned err: %v", err)
	}
	if resp.Decision != riskv1.Decision_ALLOW {
		t.Fatalf("shadow should ALLOW, got %v", resp.Decision)
	}
	if resp.Reason != "shadow:bypass" {
		t.Fatalf("expected reason shadow:bypass, got %q", resp.Reason)
	}
	if resp.RiskLevel != "shadow" {
		t.Fatalf("expected risk_level=shadow, got %q", resp.RiskLevel)
	}
}

func TestBulkScreen_ShadowBypass(t *testing.T) {
	s := newShadowOnlyServer()
	ctx := shadow.WithShadow(context.Background(), true)
	in := []*riskv1.ScreenRequest{
		{PaymentIntentId: "pi_1"},
		{PaymentIntentId: "pi_2"},
		{PaymentIntentId: "pi_3"},
	}
	resp, err := s.BulkScreen(ctx, &riskv1.BulkScreenRequest{Requests: in})
	if err != nil {
		t.Fatalf("BulkScreen err: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("want 3 results, got %d", len(resp.Results))
	}
	for i, r := range resp.Results {
		if r.Index != int32(i) {
			t.Errorf("result[%d].index = %d", i, r.Index)
		}
		if r.Response == nil || r.Response.Decision != riskv1.Decision_ALLOW {
			t.Errorf("result[%d] not allowed: %+v", i, r.Response)
		}
		if r.Error != "" {
			t.Errorf("result[%d] has unexpected error: %s", i, r.Error)
		}
	}
}

func TestReport_ShadowBypass(t *testing.T) {
	s := newShadowOnlyServer()
	ctx := shadow.WithShadow(context.Background(), true)
	resp, err := s.Report(ctx, &riskv1.ReportRequest{
		MerchantId:      "any",
		PaymentIntentId: "pi_load",
		EventType:       "succeeded",
	})
	if err != nil {
		t.Fatalf("Report err: %v", err)
	}
	if resp == nil {
		t.Fatal("Report returned nil resp")
	}
	// 关键：svc=nil 没 panic 即证明没进入 svc.Report 真实路径
}

// 反向：非 shadow ctx 时，svc=nil 会触发 nil deref（说明 short-circuit 没误开）。
// 用 recover 验证而不是真让它 panic 把测试干掉。
func TestScreen_NonShadowFallsThrough(t *testing.T) {
	s := newShadowOnlyServer()
	defer func() {
		// 期望进入下面的真实路径（任意一处 nil deref / 空鉴权失败）→ 一定不会
		// 安然返回 ALLOW with reason=shadow:bypass。
		if r := recover(); r != nil {
			// 进入了真实路径，nil panic 符合预期 — 证明短路只对 shadow 生效
			return
		}
	}()
	resp, err := s.Screen(context.Background(), &riskv1.ScreenRequest{})
	// 没 panic 时检查：response 绝不能是 shadow 短路那个
	if err == nil && resp != nil && resp.Reason == "shadow:bypass" {
		t.Fatal("non-shadow ctx should NOT take shadow:bypass path")
	}
}
