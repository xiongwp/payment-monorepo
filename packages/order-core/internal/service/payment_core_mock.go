package service

import (
	"context"
	"strings"
	"time"

	"github.com/xiongwp/order-core/internal/channel"
)

// MockPaymentCoreChannel 模拟 payment-core 系统的 PaymentChannel 实现。
//
// 真实架构：
//
//	order-core ──gRPC──→ payment-core ──→ 各支付渠道（Stripe / Alipay / GCash / ...）
//
// payment-core 内部：
//   - 按 payment_method（VISA / BALANCE / ALIPAY / WECHAT / GCASH / GRABPAY / SHOPEEPAY ...）
//     走支付路由选择具体 acquirer
//   - 处理各渠道 API 细节、签名、webhook 解析
//   - 把渠道结果规范化后返回给 order-core
//
// 本文件的 mock 只供本地开发 / 单测使用：
//   - payment_method 含 "VISA" → 返回 requires_action + 3DS redirect
//   - 含 "BALANCE"             → 返回 requires_action + OTP
//   - 含 "GCASH" / "ALIPAY"    → 返回 requires_action + App redirect
//   - 含 "FAIL"                → 返回 failed
//   - 其它                     → 同步 succeeded
type MockPaymentCoreChannel struct{}

// NewMockPaymentCoreChannel 构造
func NewMockPaymentCoreChannel() *MockPaymentCoreChannel { return &MockPaymentCoreChannel{} }

// Name 标识（payment-core 对应的渠道名）
func (m *MockPaymentCoreChannel) Name() string { return "payment-core-mock" }

// Charge 模拟扣款
func (m *MockPaymentCoreChannel) Charge(_ context.Context, req channel.PaymentRequest) (*channel.PaymentResponse, error) {
	pm := strings.ToUpper(req.PaymentMethod)
	externalRef := "pc_mock_" + req.PaymentIntentID + "_" + time.Now().Format("20060102150405")

	switch {
	case strings.Contains(pm, "FAIL"):
		return &channel.PaymentResponse{
			ResultType:     channel.PaymentResultFailed,
			ExternalRefNo:  externalRef,
			FailureCode:    "card_declined",
			FailureMessage: "mock: forced failure",
		}, nil
	case strings.Contains(pm, "VISA") || strings.Contains(pm, "MASTERCARD"):
		return &channel.PaymentResponse{
			ResultType:    channel.PaymentResultRequiresAction,
			ExternalRefNo: externalRef,
			RequiredAction: channel.NewThreeDSRequiredAction(channel.ThreeDSDetails{
				RedirectURL: "https://payment-core.example.com/3ds/" + externalRef,
				SessionID:   "ds_" + externalRef,
				ReturnURL:   req.ReturnURL,
			}, time.Now().Add(10*time.Minute)),
		}, nil
	case strings.Contains(pm, "BALANCE"):
		return &channel.PaymentResponse{
			ResultType:    channel.PaymentResultRequiresAction,
			ExternalRefNo: externalRef,
			RequiredAction: channel.NewOTPRequiredAction(channel.OTPDetails{
				RecipientMasked: "+86 ***1234",
				Channel:         "sms",
				Length:          6,
				ResendAfterSec:  60,
			}, "123456", time.Now().Add(5*time.Minute)),
		}, nil
	case strings.Contains(pm, "GCASH") || strings.Contains(pm, "GRABPAY") || strings.Contains(pm, "SHOPEEPAY") || strings.Contains(pm, "ALIPAY") || strings.Contains(pm, "WECHAT"):
		return &channel.PaymentResponse{
			ResultType:    channel.PaymentResultRequiresAction,
			ExternalRefNo: externalRef,
			RequiredAction: channel.NewAppRedirectRequiredAction(channel.AppRedirectDetails{
				RedirectURL: "alipays://pay?biz_id=" + externalRef,
				Scheme:      "ios",
				ReturnURL:   req.ReturnURL,
			}, time.Now().Add(15*time.Minute)),
		}, nil
	}
	return &channel.PaymentResponse{
		ResultType:     channel.PaymentResultSucceeded,
		ExternalRefNo:  externalRef,
		AuthCode:       "AUTH000",
		AmountCaptured: req.Amount,
	}, nil
}

// Capture 模拟捕获
func (m *MockPaymentCoreChannel) Capture(_ context.Context, req channel.CaptureRequest) (*channel.CaptureResponse, error) {
	return &channel.CaptureResponse{
		ResultType:     channel.PaymentResultSucceeded,
		ExternalRefNo:  req.ExternalRefNo,
		AmountCaptured: req.Amount,
	}, nil
}

// Void 模拟撤销
func (m *MockPaymentCoreChannel) Void(_ context.Context, req channel.VoidRequest) (*channel.VoidResponse, error) {
	return &channel.VoidResponse{ResultType: channel.PaymentResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// Refund 模拟退款（同步成功）
func (m *MockPaymentCoreChannel) Refund(_ context.Context, req channel.RefundChannelRequest) (*channel.RefundChannelResponse, error) {
	return &channel.RefundChannelResponse{
		ResultType:          channel.PaymentResultSucceeded,
		ExternalRefundRefNo: "rf_" + req.RefundID,
	}, nil
}

// Query 模拟查询：总是返回已支付
func (m *MockPaymentCoreChannel) Query(_ context.Context, req channel.QueryRequest) (*channel.QueryResponse, error) {
	return &channel.QueryResponse{
		ResultType:     channel.PaymentResultSucceeded,
		ExternalRefNo:  req.ExternalRefNo,
		AmountCaptured: 0,
	}, nil
}

// ParseWebhook 从原始 body 里取 event_id / event_type / pi_id / charge_id / refund_id。
// 真实 payment-core 会做签名校验；mock 只做字段提取。
func (m *MockPaymentCoreChannel) ParseWebhook(_ context.Context, _ map[string]string, body []byte) (*channel.WebhookEvent, error) {
	// 期望 mock body 是 k1=v1&k2=v2 形式（本地/测试手写方便）
	fields := map[string]string{}
	for _, kv := range strings.Split(string(body), "&") {
		if i := strings.IndexByte(kv, '='); i > 0 {
			fields[kv[:i]] = kv[i+1:]
		}
	}
	return &channel.WebhookEvent{
		EventID:         fields["event_id"],
		EventType:       fields["event_type"],
		PaymentIntentID: fields["pi_id"],
		ChargeID:        fields["charge_id"],
		RefundID:        fields["refund_id"],
		ExternalRefNo:   fields["external_ref_no"],
		RawPayload:      fields,
		Timestamp:       time.Now().UTC(),
	}, nil
}
