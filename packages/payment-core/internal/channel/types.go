// Package channel 内部用于在 payment-core 里搬运的类型（与 order-core 的
// internal/channel 几乎完全一致；在本仓独立定义是为了避免引入 order-core 的
// 依赖循环，gRPC wire 层做双向映射）。
package channel

import "time"

type PaymentResultType string

const (
	ResultSucceeded      PaymentResultType = "succeeded"
	ResultAuthorized     PaymentResultType = "authorized"
	ResultProcessing     PaymentResultType = "processing"
	ResultRequiresAction PaymentResultType = "requires_action"
	ResultFailed         PaymentResultType = "failed"
)

type RequiredActionType string

const (
	ActionTypeThreeDS            RequiredActionType = "three_d_secure"
	ActionTypeOTP                RequiredActionType = "otp"
	ActionTypePayPassword        RequiredActionType = "pay_password"
	ActionTypeQRCode             RequiredActionType = "qrcode"
	ActionTypeAppRedirect        RequiredActionType = "app_redirect"
	// ActionTypeMiniProgramInvoke 小程序内 JSAPI 唤起支付（GCash mini-app /
	// WeChat Pay 小程序 / Alipay 小程序）。前端在小程序内拿到 Details 里的
	// payment_id 后调用渠道 SDK 的 my.tradePay / wx.requestPayment 等 JSAPI；
	// 没有 redirect_url，跳转语义不适用。
	ActionTypeMiniProgramInvoke  RequiredActionType = "mini_program_invoke"
)

// PaymentRequest 由 order-core 发给 payment-core 的扣款请求。
type PaymentRequest struct {
	PaymentIntentID  string
	ChargeID         string
	Amount           int64
	Currency         string
	Country          string
	PaymentMethod    string
	PaymentMethodRef string
	CustomerID       string
	CaptureMethod    string
	ReturnURL        string
	NotifyURL        string
	Description      string
	Metadata         map[string]string
	Extra            map[string]string
}

type RequiredAction struct {
	Type         RequiredActionType
	Details      map[string]string
	ChallengeRef string
	ExpiresAt    time.Time
}

type PaymentResponse struct {
	ResultType       PaymentResultType
	ExternalRefNo    string
	AuthCode         string
	AmountAuthorized int64
	AmountCaptured   int64
	RequiredAction   *RequiredAction
	FailureCode      string
	FailureMessage   string
	RiskLevel        string
	RiskScore        int
	RawResponse      map[string]string
}

type CaptureRequest struct {
	PaymentIntentID string
	ChargeID        string
	ExternalRefNo   string
	Amount          int64
	Extra           map[string]string
}

type CaptureResponse struct {
	ResultType     PaymentResultType
	ExternalRefNo  string
	AmountCaptured int64
	FailureCode    string
	FailureMessage string
	RawResponse    map[string]string
}

type VoidRequest struct {
	PaymentIntentID string
	ChargeID        string
	ExternalRefNo   string
	Reason          string
	Extra           map[string]string
}

type VoidResponse struct {
	ResultType     PaymentResultType
	ExternalRefNo  string
	FailureCode    string
	FailureMessage string
	RawResponse    map[string]string
}

type RefundChannelRequest struct {
	PaymentIntentID string
	ChargeID        string
	RefundID        string
	ExternalRefNo   string
	Amount          int64
	Currency        string
	Reason          string
	Extra           map[string]string
}

type RefundChannelResponse struct {
	ResultType          PaymentResultType
	ExternalRefundRefNo string
	FailureCode         string
	FailureMessage      string
	RawResponse         map[string]string
}

type QueryRequest struct {
	PaymentIntentID string
	ChargeID        string
	ExternalRefNo   string
	Extra           map[string]string
}

type QueryResponse struct {
	ResultType     PaymentResultType
	ExternalRefNo  string
	AmountCaptured int64
	AmountRefunded int64
	FailureCode    string
	FailureMessage string
	RawResponse    map[string]string
}

type WebhookEvent struct {
	EventID         string
	EventType       string
	PaymentIntentID string
	ChargeID        string
	RefundID        string
	ExternalRefNo   string
	Amount          int64
	Timestamp       time.Time
	RawPayload      map[string]string
}
