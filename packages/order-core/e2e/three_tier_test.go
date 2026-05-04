//go:build e2e

// 真 · 三层端到端测试：order-core → payment-core → payment-channel → fake 外部渠道。
//
// 三个仓库都启动真实的 gRPC 服务（在 bufconn 上），只有最底层的"第三方渠道
// HTTP API"（GCash / Maya / 银行）是 mock。
//
// 运行前置：
//  1. 同级克隆了 payment-core 和 payment-channel
//  2. /home/user/go.work 把三个模块加进 workspace
//
// 跑法：
//   cd /home/user && go test -tags=e2e ./order-core/e2e/...
package e2e

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pchanth "github.com/xiongwp/payment-channel/testhelper"
	pcth "github.com/xiongwp/payment-core/testhelper"

	ordchannel "github.com/xiongwp/order-core/internal/channel"
)

// stack 一次启动三层：payment-channel → payment-core → order-core 的 PaymentChannel impl。
// 外部渠道完全 mock 在 pchanth.ScriptedAdapter 里。
type stack struct {
	pchan *pchanth.Server
	pcore *pcth.Server
	pc    ordchannel.PaymentChannel
}

func spinUp(t testing.TB) *stack {
	t.Helper()

	// (1) payment-channel：真实服务 + 三个 scripted PH adapter
	pchan := pchanth.Start(t,
		pchanth.ScriptedAdapter{
			Name: "gcash",
			Charge: pchanth.ScriptedChargeResult{
				Result:      "requires_action",
				RedirectURL: "gcash://pay?token=xxx",
				ExpiresAt:   time.Now().Add(15 * time.Minute),
			},
		},
		pchanth.ScriptedAdapter{
			Name: "maya",
			Charge: pchanth.ScriptedChargeResult{
				Result:      "requires_action",
				RedirectURL: "https://payments.paymaya.com/checkout?id=co_1",
				ExpiresAt:   time.Now().Add(30 * time.Minute),
			},
		},
		pchanth.ScriptedAdapter{
			Name: "instapay",
			Charge: pchanth.ScriptedChargeResult{
				Result:        "succeeded",
				ExternalRefNo: "IP_fixed",
			},
		},
	)

	// (2) payment-core：真实服务，channelclient 指到 payment-channel 的 bufconn
	pcore := pcth.Start(t, pcth.StartConfig{
		ChannelConn: pchan.Conn,
		Rules: []pcth.Rule{
			{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
			{Priority: 100, Country: "PH", PaymentMethod: "MAYA", Adapter: "maya"},
			{Priority: 100, Country: "PH", PaymentMethod: "INSTAPAY", Adapter: "instapay"},
			{Priority: 999, Country: "PH", Adapter: "gcash"},
		},
	})

	// (3) order-core 侧：PaymentCoreGRPCChannel 拨号 payment-core 的 bufconn
	pc := NewPaymentCoreGRPCChannel("payment-core", pcore.Conn)
	return &stack{pchan: pchan, pcore: pcore, pc: pc}
}

// ─── tests ─────────────────────────────────────────────────────

func TestThreeTier_GCash_EndToEnd(t *testing.T) {
	st := spinUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
		PaymentIntentID: "pi_4471234560001",
		ChargeID:        "ch_4471234560001",
		Amount:          10000,
		Currency:        "PHP",
		PaymentMethod:   "GCASH",
		ReturnURL:       "https://cashier.example.com/ret",
		Metadata:        map[string]string{"country": "PH"},
	})
	if err != nil {
		t.Fatalf("three-tier charge failed: %v", err)
	}
	if resp.ResultType != ordchannel.PaymentResultRequiresAction {
		t.Fatalf("want requires_action got %s", resp.ResultType)
	}
	if resp.ExternalRefNo != "gcash_pi_4471234560001" {
		t.Fatalf("external_ref did not propagate: %q", resp.ExternalRefNo)
	}
	if resp.RequiredAction == nil {
		t.Fatal("required_action must flow back to order-core")
	}
	if resp.RequiredAction.Type != ordchannel.RequiredActionAppRedirect {
		t.Fatalf("want app_redirect got %s", resp.RequiredAction.Type)
	}
	if got := resp.RequiredAction.Details["redirect_url"]; got != "gcash://pay?token=xxx" {
		t.Fatalf("redirect_url lost: %q", got)
	}
	if got := resp.RequiredAction.Details["return_url"]; got != "https://cashier.example.com/ret" {
		t.Fatalf("return_url from order-core lost: %q", got)
	}
}

func TestThreeTier_Maya_RedirectPropagates(t *testing.T) {
	st := spinUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
		PaymentIntentID: "pi_5571234560001",
		Amount:          20000, Currency: "PHP", PaymentMethod: "MAYA",
		ReturnURL: "https://cashier.example.com/ret?r=maya",
		Metadata:  map[string]string{"country": "PH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ResultType != ordchannel.PaymentResultRequiresAction {
		t.Fatalf("maya should be requires_action got %s", resp.ResultType)
	}
	if !strings.Contains(resp.RequiredAction.Details["redirect_url"], "paymaya.com") {
		t.Fatalf("maya redirect url lost: %v", resp.RequiredAction.Details)
	}
}

func TestThreeTier_InstaPay_Sync(t *testing.T) {
	st := spinUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
		PaymentIntentID: "pi_1171234560001",
		Amount:          4000000, Currency: "PHP", PaymentMethod: "INSTAPAY",
		Metadata: map[string]string{"country": "PH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ResultType != ordchannel.PaymentResultSucceeded {
		t.Fatalf("instapay sync should succeed, got %s", resp.ResultType)
	}
	if resp.ExternalRefNo != "IP_fixed" {
		t.Fatalf("bank ref should be IP_fixed, got %s", resp.ExternalRefNo)
	}
}

func TestThreeTier_Refund_WithRoutingHint(t *testing.T) {
	st := spinUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 先下一笔 GCash 订单
	charge, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
		PaymentIntentID: "pi_4471234560001",
		Amount:          10000, Currency: "PHP", PaymentMethod: "GCASH",
		Metadata: map[string]string{"country": "PH"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 退款：order-core 必须把 adapter 名通过 Extra 透传，否则 payment-core 拒绝
	rfd, err := st.pc.Refund(ctx, ordchannel.RefundChannelRequest{
		PaymentIntentID: "pi_4471234560001",
		RefundID:        "re_4471234560001",
		ExternalRefNo:   charge.ExternalRefNo,
		Amount:          5000,
		Currency:        "PHP",
		Extra:           map[string]string{"adapter": "gcash"},
	})
	if err != nil {
		t.Fatalf("refund with adapter hint should succeed: %v", err)
	}
	if rfd.ResultType != ordchannel.PaymentResultSucceeded {
		t.Fatalf("want refund succeeded got %s", rfd.ResultType)
	}
	if !strings.HasPrefix(rfd.ExternalRefundRefNo, "rfd_") {
		t.Fatalf("refund ref wrong: %s", rfd.ExternalRefundRefNo)
	}
}

func TestThreeTier_Refund_RejectsWithoutRoutingHint(t *testing.T) {
	st := spinUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := st.pc.Refund(ctx, ordchannel.RefundChannelRequest{
		PaymentIntentID: "pi_4471234560001",
		RefundID:        "re_4471234560001",
		ExternalRefNo:   "gcash_ref",
		Amount:          1000,
		Currency:        "PHP",
	})
	if err == nil {
		t.Fatal("refund without extra[adapter] should be rejected by payment-core")
	}
}

// TestThreeTier_GCashMiniProgram_EndToEnd 三层端到端：商户 SDK 用
// payment_method=GCASH_MINIPROGRAM 触发小程序 JSAPI 流程：
//   - payment-core 看到 GCASH_MINIPROGRAM 自动注入 metadata[gcash_flow]=miniprogram
//   - payment-channel 的 gcash adapter（这里用捕获式 scripted）确认收到该 metadata，
//     然后返回 mini_program_invoke action（含 payment_id / partner_id / sign_method）
//   - order-core 收到的 RequiredAction.Type == mini_program_invoke，
//     Details 里能取到 payment_id 给小程序前端 my.tradePay 用
func TestThreeTier_GCashMiniProgram_EndToEnd(t *testing.T) {
	var (
		captureMu sync.Mutex
		gotCharge *pchanth.CapturedCharge
	)

	pchan := pchanth.Start(t, pchanth.ScriptedAdapter{
		Name: "gcash",
		Charge: pchanth.ScriptedChargeResult{
			Result:        "requires_action",
			ExternalRefNo: "GCASH-PAY-MP-001",
			RequiredAction: &pchanth.ScriptedRequiredAction{
				Type:      string(ordchannel.RequiredActionMiniProgramInvoke),
				ExpiresAt: time.Now().Add(15 * time.Minute),
				Extra: map[string]string{
					"payment_id":  "GCASH-PAY-MP-001",
					"partner_id":  "P-MP-PARTNER",
					"sign_method": "RSA256",
				},
			},
		},
		OnCharge: func(c pchanth.CapturedCharge) {
			captureMu.Lock()
			defer captureMu.Unlock()
			cp := c // value copy; CapturedCharge.Metadata is already a copy made by testhelper
			gotCharge = &cp
		},
	})

	pcore := pcth.Start(t, pcth.StartConfig{
		ChannelConn: pchan.Conn,
		Rules: []pcth.Rule{
			// 注意：先放更具体的 GCASH_MINIPROGRAM，再 GCASH 兜底
			{Priority: 100, Country: "PH", PaymentMethod: "GCASH_MINIPROGRAM", Adapter: "gcash"},
			{Priority: 200, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
		},
	})
	pc := NewPaymentCoreGRPCChannel("payment-core", pcore.Conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := pc.Charge(ctx, ordchannel.PaymentRequest{
		PaymentIntentID: "pi_mp1234567890",
		ChargeID:        "ch_mp1234567890",
		Amount:          50000,
		Currency:        "PHP",
		PaymentMethod:   "GCASH_MINIPROGRAM",
		Metadata:        map[string]string{"country": "PH"},
	})
	if err != nil {
		t.Fatalf("mini-program charge failed: %v", err)
	}

	if resp.ResultType != ordchannel.PaymentResultRequiresAction {
		t.Fatalf("want requires_action, got %s", resp.ResultType)
	}
	if resp.RequiredAction == nil {
		t.Fatal("RequiredAction must propagate to order-core")
	}
	if resp.RequiredAction.Type != ordchannel.RequiredActionMiniProgramInvoke {
		t.Fatalf("Type = %s, want mini_program_invoke", resp.RequiredAction.Type)
	}
	// Details 字段——前端 SDK 据此调 my.tradePay({paymentId})
	if got := resp.RequiredAction.Details["payment_id"]; got != "GCASH-PAY-MP-001" {
		t.Fatalf("payment_id lost across stack: %q", got)
	}
	if got := resp.RequiredAction.Details["partner_id"]; got != "P-MP-PARTNER" {
		t.Fatalf("partner_id lost: %q", got)
	}
	if got := resp.RequiredAction.Details["sign_method"]; got != "RSA256" {
		t.Fatalf("sign_method lost: %q", got)
	}
	// 解析强类型回结构
	mpd := ordchannel.ParseMiniProgramInvoke(resp.RequiredAction)
	if mpd.PaymentID != "GCASH-PAY-MP-001" || mpd.PartnerID != "P-MP-PARTNER" || mpd.SignMethod != "RSA256" {
		t.Fatalf("ParseMiniProgramInvoke roundtrip failed: %+v", mpd)
	}

	// 验证 payment-core 把 metadata[gcash_flow]=miniprogram 自动注入到了
	// 流到 payment-channel 的请求里——这是商户 SDK 不需要显式传该 flag 的关键。
	captureMu.Lock()
	defer captureMu.Unlock()
	if gotCharge == nil {
		t.Fatal("scripted adapter never received a charge")
	}
	if gotCharge.Metadata["gcash_flow"] != "miniprogram" {
		t.Fatalf("metadata gcash_flow not auto-injected by payment-core; got metadata=%+v", gotCharge.Metadata)
	}
	// 商户原本传的 metadata 仍要保留
	if gotCharge.Metadata["country"] != "PH" {
		t.Fatalf("merchant-supplied metadata stripped: %+v", gotCharge.Metadata)
	}
}

// TestThreeTier_GCashMiniProgram_RespectsCallerMetadata 当商户 SDK 已经
// 显式传了 gcash_flow 标志时，payment-core 不应覆盖该值。
func TestThreeTier_GCashMiniProgram_RespectsCallerMetadata(t *testing.T) {
	var (
		captureMu sync.Mutex
		gotCharge *pchanth.CapturedCharge
	)
	pchan := pchanth.Start(t, pchanth.ScriptedAdapter{
		Name: "gcash",
		Charge: pchanth.ScriptedChargeResult{
			Result:        "succeeded",
			ExternalRefNo: "GCASH-DIRECT",
		},
		OnCharge: func(c pchanth.CapturedCharge) {
			captureMu.Lock()
			defer captureMu.Unlock()
			cp := c
			gotCharge = &cp
		},
	})
	pcore := pcth.Start(t, pcth.StartConfig{
		ChannelConn: pchan.Conn,
		Rules: []pcth.Rule{
			{Priority: 100, Country: "PH", PaymentMethod: "GCASH_MINIPROGRAM", Adapter: "gcash"},
		},
	})
	pc := NewPaymentCoreGRPCChannel("payment-core", pcore.Conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pc.Charge(ctx, ordchannel.PaymentRequest{
		PaymentIntentID: "pi_mp_caller_999",
		Amount:          12345, Currency: "PHP", PaymentMethod: "GCASH_MINIPROGRAM",
		Metadata: map[string]string{
			"country":    "PH",
			"gcash_flow": "OVERRIDDEN", // 商户故意传一个非默认值
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	captureMu.Lock()
	defer captureMu.Unlock()
	if gotCharge == nil {
		t.Fatal("scripted adapter never received a charge")
	}
	if gotCharge.Metadata["gcash_flow"] != "OVERRIDDEN" {
		t.Fatalf("payment-core overrode caller-supplied gcash_flow; got %q", gotCharge.Metadata["gcash_flow"])
	}
}

func TestThreeTier_IdempotentReplay_NoDoubleCharge(t *testing.T) {
	st := spinUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 同一笔支付被 order-core 重发两次（网络抖动场景）。
	// payment-channel 的 UNIQUE(adapter, idempotency_key) 兜底：第二次不会真正
	// 调用 scripted adapter，而是把第一次的响应回放回来。
	req := ordchannel.PaymentRequest{
		PaymentIntentID: "pi_8871234560001",
		Amount:          99999, Currency: "PHP", PaymentMethod: "GCASH",
		Metadata: map[string]string{"country": "PH"},
	}
	r1, err := st.pc.Charge(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := st.pc.Charge(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ExternalRefNo != r2.ExternalRefNo {
		t.Fatalf("idempotent replay should return same ref: %q vs %q", r1.ExternalRefNo, r2.ExternalRefNo)
	}
	if r1.ResultType != r2.ResultType {
		t.Fatalf("idempotent replay should return same result type: %s vs %s", r1.ResultType, r2.ResultType)
	}
}
