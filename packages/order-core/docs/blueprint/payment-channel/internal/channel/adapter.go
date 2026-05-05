//go:build ignore
// +build ignore

// Package channel 定义 payment-channel 面向 payment-core 的 Adapter 接口。
// 每个真实渠道（gcash/maya/grabpay/coinsph/instapay/pesonet）实现此接口。
package channel

import (
	"context"
	"time"
)

// Adapter 每个 PH 渠道实现；形态与 order-core 的 channel.PaymentChannel 一致，
// 但这里每个方法只对一种固定渠道生效，由外层按 payment_method 选择。
type Adapter interface {
	Name() string
	Charge(ctx context.Context, req *ChargeRequest) (*ChargeResponse, error)
	Capture(ctx context.Context, req *CaptureRequest) (*OpResponse, error)
	Void(ctx context.Context, req *VoidRequest) (*OpResponse, error)
	Refund(ctx context.Context, req *RefundRequest) (*OpResponse, error)
	Query(ctx context.Context, req *QueryRequest) (*QueryResponse, error)
	ParseWebhook(headers map[string]string, body []byte) (*WebhookEvent, error)
}

// ---- Requests --------------------------------------------------------------

type ChargeRequest struct {
	PiID            string
	IdempotencyKey  string // 由 payment-core 透传
	Amount          int64  // 分
	Currency        string // PHP
	Buyer           Buyer
	Description     string
	ReturnURL       string
	NotifyURL       string // payment-channel 自己的 webhook 入口
	CaptureImmediate bool
	Metadata        map[string]string
}

type CaptureRequest struct {
	PiID           string
	ExternalRefNo  string
	Amount         int64
	IdempotencyKey string
}

type VoidRequest struct {
	PiID           string
	ExternalRefNo  string
	IdempotencyKey string
}

type RefundRequest struct {
	PiID           string
	ExternalRefNo  string // 原 charge 的渠道流水号
	Amount         int64
	Reason         string
	IdempotencyKey string
}

type QueryRequest struct {
	PiID          string
	ExternalRefNo string
}

type Buyer struct {
	FirstName string
	LastName  string
	Email     string
	Phone     string
}

// ---- Responses -------------------------------------------------------------

type ResultType string

const (
	ResultSucceeded       ResultType = "succeeded"
	ResultAuthorized      ResultType = "authorized"
	ResultProcessing      ResultType = "processing"
	ResultRequiresAction  ResultType = "requires_action"
	ResultFailed          ResultType = "failed"
)

type ChargeResponse struct {
	Result          ResultType
	ExternalRefNo   string
	RequiredAction  *RequiredAction // 非 nil 表示 RequiresAction
	FailureCode     string          // 归一后的失败码
	RawFailureCode  string
	FailureMessage  string
	Raw             map[string]string
}

type OpResponse struct {
	Result         ResultType
	ExternalRefNo  string
	FailureCode    string
	RawFailureCode string
	FailureMessage string
}

type QueryResponse struct {
	Result         ResultType
	ExternalRefNo  string
	AmountCaptured int64
	AmountRefunded int64
	Raw            map[string]string
}

// RequiredAction 保持与 order-core 的 channel.RequiredAction 同构，payment-core
// 负责做中间转换。细分结构见 order-core/internal/channel/required_actions.go。
type RequiredAction struct {
	Type        string // three_d_secure / otp / pay_password / qr_code / app_redirect
	ExpiresAt   time.Time
	RedirectURL string
	Scheme      string
	ReturnURL   string
	QRCodeURL   string
	QRImageB64  string
	PollMs      int
	OTPMasked   string
	OTPChannel  string
	OTPLength   int
	Extra       map[string]string
}

// ---- Webhook ---------------------------------------------------------------

type WebhookEvent struct {
	EventID       string
	EventType     string // charge.succeeded / charge.failed / refund.succeeded / ...
	PiID          string
	ExternalRefNo string
	Amount        int64
	Timestamp     time.Time
	SignatureOK   bool
	Raw           map[string]string
}