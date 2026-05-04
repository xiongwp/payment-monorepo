package channel

import (
	"context"
	"time"
)

// ─── 枚举 ─────────────────────────────────────────────────────────────────────

// PaymentResultType 渠道扣款 / 捕获的结果类型
type PaymentResultType string

const (
	// PaymentResultSucceeded 同步扣款成功（自动捕获场景）
	PaymentResultSucceeded PaymentResultType = "succeeded"
	// PaymentResultAuthorized 预授权成功，等待 Capture（手动捕获场景）
	PaymentResultAuthorized PaymentResultType = "authorized"
	// PaymentResultProcessing 异步受理中，等渠道 webhook 或轮询确认（Alipay 扫码、BankTransfer 等）
	PaymentResultProcessing PaymentResultType = "processing"
	// PaymentResultRequiresAction 需要用户完成动作（3DS 跳转 / OTP / 支付密码 / 扫二维码）
	PaymentResultRequiresAction PaymentResultType = "requires_action"
	// PaymentResultFailed 同步终态失败
	PaymentResultFailed PaymentResultType = "failed"
)

// RequiredActionType 渠道要求用户完成的动作类型
type RequiredActionType string

const (
	RequiredAction3DSRedirect       RequiredActionType = "three_d_secure"      // 跳转 ACS
	RequiredActionOTP               RequiredActionType = "otp"                 // 输入一次性验证码
	RequiredActionPayPassword       RequiredActionType = "pay_password"        // 输入支付密码
	RequiredActionQRCodeScan        RequiredActionType = "qrcode"              // 用户扫码
	RequiredActionAppRedirect       RequiredActionType = "app_redirect"        // 跳转 App（如支付宝 / 微信）
	// RequiredActionMiniProgramInvoke 在小程序内 JSAPI 唤起支付。
	// 商户/用户已经在宿主 App（GCash / WeChat / Alipay）内的小程序里，
	// 不需要跳转——前端用 Details["payment_id"] 调宿主 SDK 的 my.tradePay /
	// wx.requestPayment 等 JSAPI 即可。RedirectURL 字段不适用。
	RequiredActionMiniProgramInvoke RequiredActionType = "mini_program_invoke"
)

// ─── 请求 / 响应 ──────────────────────────────────────────────────────────────

// PaymentRequest 向外部渠道发起扣款的入参
type PaymentRequest struct {
	PaymentIntentID  string
	ChargeID         string
	Amount           int64
	Currency         string
	PaymentMethod    string // VISA / BALANCE / ALIPAY / WECHAT / ...
	PaymentMethodRef string // token / card_token / wallet_id / account_no
	CustomerID       string
	CaptureMethod    string // automatic / manual
	ReturnURL        string // 用户完成跳转/挑战后的回跳 URL
	NotifyURL        string // 渠道异步 webhook 回调地址
	Description      string
	Metadata         map[string]string
	// Extra 渠道特定字段，由具体 adapter 解析（如 Alipay 的 store_id、分期参数等）
	Extra map[string]string
}

// RequiredAction 渠道要求用户完成的动作
type RequiredAction struct {
	Type RequiredActionType
	// Details 给前端展示 / 跳转的字段：
	//   3DS:         {redirect_url, session_id}
	//   OTP:         {recipient_masked, channel, challenge_id}
	//   PayPassword: {algorithm}
	//   QRCode:      {qrcode_url, qrcode_image_base64}
	//   AppRedirect: {redirect_url, scheme}
	Details map[string]string
	// ChallengeRef 服务端保留的渠道挑战引用（session_id / otp_challenge_id 等），Verify 时反查用
	ChallengeRef string
	// ExpiresAt 挑战过期时间，零值由上层用默认 TTL 兜底
	ExpiresAt time.Time
}

// PaymentResponse 渠道返回的扣款结果
type PaymentResponse struct {
	ResultType PaymentResultType

	// ExternalRefNo 渠道返回的交易号（对账核心字段）
	ExternalRefNo string
	// AuthCode 银行授权码（卡支付场景）
	AuthCode string
	// AmountAuthorized / AmountCaptured 预授权 / 捕获金额（可不同）
	AmountAuthorized int64
	AmountCaptured   int64

	// RequiredAction 当 ResultType=RequiresAction 时必填
	RequiredAction *RequiredAction

	// FailureCode / FailureMessage 当 ResultType=Failed 时填写
	FailureCode    string
	FailureMessage string

	// 风控信息（可空）
	RiskLevel string // normal / elevated / highest
	RiskScore int    // 0-100

	// RawResponse 渠道原始响应片段，供排查 / 审计
	RawResponse map[string]string
}

// CaptureRequest 预授权捕获入参
type CaptureRequest struct {
	PaymentIntentID string
	ChargeID        string
	ExternalRefNo   string // Charge 时渠道返回的交易号
	Amount          int64  // 0 表示全额捕获
	Extra           map[string]string
}

// CaptureResponse 预授权捕获结果
type CaptureResponse struct {
	ResultType     PaymentResultType // 通常是 Succeeded / Failed / Processing
	ExternalRefNo  string
	AmountCaptured int64
	FailureCode    string
	FailureMessage string
	RawResponse    map[string]string
}

// VoidRequest 撤销预授权入参
type VoidRequest struct {
	PaymentIntentID string
	ChargeID        string
	ExternalRefNo   string
	Reason          string
	Extra           map[string]string
}

// VoidResponse 撤销预授权结果
type VoidResponse struct {
	ResultType     PaymentResultType
	ExternalRefNo  string
	FailureCode    string
	FailureMessage string
	RawResponse    map[string]string
}

// RefundChannelRequest 渠道侧退款入参（区别于 domain.Refund）
type RefundChannelRequest struct {
	PaymentIntentID string
	ChargeID        string
	RefundID        string
	ExternalRefNo   string // 原交易号
	Amount          int64
	Currency        string
	Reason          string
	Extra           map[string]string
}

// RefundChannelResponse 渠道侧退款结果
type RefundChannelResponse struct {
	ResultType          PaymentResultType // Succeeded / Processing / Failed
	ExternalRefundRefNo string
	FailureCode         string
	FailureMessage      string
	RawResponse         map[string]string
}

// QueryRequest 查询渠道交易状态（对账 / 兜底）
type QueryRequest struct {
	PaymentIntentID string
	ChargeID        string
	ExternalRefNo   string
}

// QueryResponse 查询结果
type QueryResponse struct {
	ResultType     PaymentResultType
	ExternalRefNo  string
	AmountCaptured int64
	AmountRefunded int64
	FailureCode    string
	FailureMessage string
	RawResponse    map[string]string
}

// WebhookEvent 渠道异步通知规范化结构
type WebhookEvent struct {
	EventID         string // 渠道事件唯一 ID（用于幂等）
	EventType       string // charge.succeeded / charge.failed / refund.succeeded / ...
	PaymentIntentID string
	ChargeID        string
	RefundID        string
	ExternalRefNo   string
	Amount          int64
	Timestamp       time.Time
	RawPayload      map[string]string
}

// ─── 主接口 ───────────────────────────────────────────────────────────────────

// PaymentChannel 外部支付渠道统一抽象。
//
// 每个渠道（VISA 走 acquirer / BALANCE 走账户系统 / ALIPAY 走支付宝开放平台 / 自建通道）
// 实现一份此接口。在启动期通过 PaymentChannelRegistry.Register 挂到 payment_method 上。
//
// 职责：
//   - Charge        发起扣款；可能同步成功、需要用户动作、异步受理或失败
//   - Capture       手动捕获预授权
//   - Void          撤销预授权（未捕获资金释放）
//   - Refund        发起退款
//   - Query         查询交易状态（对账、超时恢复）
//   - ParseWebhook  解析异步通知到规范化的 WebhookEvent
type PaymentChannel interface {
	// Name 返回渠道名（用于日志 / metrics），通常是 payment_method
	Name() string
	// Charge 发起扣款
	Charge(ctx context.Context, req PaymentRequest) (*PaymentResponse, error)
	// Capture 手动捕获（capture_method=manual）
	Capture(ctx context.Context, req CaptureRequest) (*CaptureResponse, error)
	// Void 撤销预授权
	Void(ctx context.Context, req VoidRequest) (*VoidResponse, error)
	// Refund 发起退款
	Refund(ctx context.Context, req RefundChannelRequest) (*RefundChannelResponse, error)
	// Query 查询交易状态
	Query(ctx context.Context, req QueryRequest) (*QueryResponse, error)
	// ParseWebhook 把渠道原始通知解析成规范化 WebhookEvent
	ParseWebhook(ctx context.Context, headers map[string]string, body []byte) (*WebhookEvent, error)
}

// PaymentChannelRegistry payment_method → PaymentChannel 路由
type PaymentChannelRegistry interface {
	Register(paymentMethod string, channel PaymentChannel)
	Get(paymentMethod string) PaymentChannel
	All() map[string]PaymentChannel
}
