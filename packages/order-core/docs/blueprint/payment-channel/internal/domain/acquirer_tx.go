//go:build ignore
// +build ignore

// Package domain holds payment-channel 的渠道流水实体。本仓无业务状态机 ——
// 只记录「向第三方发了什么请求，拿到什么响应，webhook 收到什么」。
package domain

import (
	"errors"
	"time"
)

// ---- AcquirerTx ------------------------------------------------------------

// AcquirerTxState 生命周期极简：pending → succeeded / failed。
type AcquirerTxState string

const (
	AcquirerTxPending   AcquirerTxState = "pending"
	AcquirerTxSucceeded AcquirerTxState = "succeeded"
	AcquirerTxFailed    AcquirerTxState = "failed"
)

// AcquirerAction 每次调用的类型。
type AcquirerAction string

const (
	ActionCharge  AcquirerAction = "charge"
	ActionCapture AcquirerAction = "capture"
	ActionVoid    AcquirerAction = "void"
	ActionRefund  AcquirerAction = "refund"
	ActionQuery   AcquirerAction = "query"
)

// AcquirerTx = 一次渠道调用的完整 request/response 快照。
type AcquirerTx struct {
	ID             uint64
	AqID           string // aq_<db><tbl><seq>
	PiID           string // 分片键
	Adapter        string
	Action         AcquirerAction
	IdempotencyKey string
	State          AcquirerTxState

	ExternalRefNo string // 渠道返回的流水号（webhook 对账用）
	Amount        int64  // 分
	Currency      string

	FailureCode    string // 归一后的码，见 channel/failure_code.go
	RawFailureCode string // 原始码

	RequestMethod  string
	RequestURL     string
	RequestHeaders map[string]string
	RequestBody    string

	ResponseStatus  int
	ResponseHeaders map[string]string
	ResponseBody    string

	LatencyMs int
	Attempt   int
	NextRetry *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ---- WebhookRaw ------------------------------------------------------------

type WebhookRaw struct {
	ID           uint64
	PiID         string
	Adapter      string
	EventID      string // 渠道给的；没有就自算 hash(body)
	EventType    string
	DedupeKey    string // sha256(adapter + ":" + event_id)
	SignatureOK  bool
	Forwarded    bool
	ForwardErr   string
	Headers      map[string]string
	Body         string
	ReceivedAt   time.Time
	ForwardedAt  *time.Time
}

// ---- ChannelToken ----------------------------------------------------------

type ChannelToken struct {
	ID          uint64
	PiID        string
	CustomerRef string
	Adapter     string
	Token       string // 落库前 AES-GCM 加密
	Brand       string
	Last4       string
	Status      string
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// ---- Errors ---------------------------------------------------------------

var (
	ErrIdempotentHit         = errors.New("idempotent hit: returning prior response")
	ErrAcquirerTxNotFound    = errors.New("acquirer_tx not found")
	ErrWebhookAlreadySeen    = errors.New("webhook already seen (dedupe hit)")
	ErrChannelUnavailable    = errors.New("channel unavailable")
	ErrChannelSignatureFail  = errors.New("channel signature verification failed")
)