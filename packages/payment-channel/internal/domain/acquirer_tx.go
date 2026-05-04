// Package domain holds payment-channel 的渠道流水实体。本仓无业务状态机 ——
// 只记录「向第三方发了什么请求，拿到什么响应，webhook 收到什么」。
package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// jsonUnmarshalWebhookHeaders is a package-local alias so the Headers()
// accessor can be covered by a test without importing encoding/json there.
func jsonUnmarshalWebhookHeaders(s string, out *map[string]string) error {
	return json.Unmarshal([]byte(s), out)
}

// ---- AcquirerTx ------------------------------------------------------------

// AcquirerTxState 生命周期：
//
//	pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
//	unknown   = 已发出请求但未拿到明确成败（网络超时/连接错/5xx），需要 Query
//	            推进 —— **绝不能在此状态直接重发原请求**，否则会重复扣款。
//	succeeded = 第三方明确返回成功
//	failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
//
// 关键：unknown ≠ failed。historically 我们把 timeout 当作 failed + 用同
// idempotency_key 重放，这是「超时即资损」的最大源头。修复后所有不可知
// 终态（网络错、超时）都落 unknown，由 PendingQueryWorker 调 Query 推进；
// 仅当 Query 也明确返回 failed 才能落终态 failed。
type AcquirerTxState string

const (
	AcquirerTxPending   AcquirerTxState = "pending"
	AcquirerTxUnknown   AcquirerTxState = "unknown"
	AcquirerTxSucceeded AcquirerTxState = "succeeded"
	AcquirerTxFailed    AcquirerTxState = "failed"
)

// IsFinal 表示状态已不会再变化 —— 用于决定是否可以加入进程内 idemCache。
// pending / unknown 是中间态，不缓存；下次查询必须走 DB / Query 推进。
func (s AcquirerTxState) IsFinal() bool {
	return s == AcquirerTxSucceeded || s == AcquirerTxFailed
}

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

	// LastQueryAt 上次 PendingQueryWorker / CallRetryWorker 调 Query 推进的时间。
	// 用于节流（单笔 unknown 行不要每个 tick 都查）。
	LastQueryAt *time.Time
	// QueryCount 已经调 Query 推进过的次数；超过上限后强制 Cancel/Void 关单。
	QueryCount int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ---- WebhookRaw ------------------------------------------------------------

type WebhookRaw struct {
	ID          uint64
	PiID        string
	Adapter     string
	EventID     string // 渠道给的；没有就自算 hash(body)
	EventType   string
	DedupeKey   string // sha256(adapter + ":" + event_id + ":" + event_type)
	SignatureOK bool
	Forwarded   bool
	ForwardErr  string
	// RawHeaders 是 DB 原始 JSON 串；Headers() 访问器按需解析（wave L perf）。
	RawHeaders  string
	Headers     map[string]string // 历史字段；用 Headers() 更稳，调用方不要直接读
	Body        string
	ReceivedAt  time.Time
	ForwardedAt *time.Time
}

// HeadersParsed lazily decodes the raw JSON headers blob. ListUnforwarded
// skips the decode per-row (hot path); callers that actually need the
// key/value view go through this accessor.
func (w *WebhookRaw) HeadersParsed() map[string]string {
	if w.Headers != nil {
		return w.Headers
	}
	if w.RawHeaders == "" {
		return nil
	}
	m := map[string]string{}
	_ = jsonUnmarshalWebhookHeaders(w.RawHeaders, &m)
	w.Headers = m
	return m
}

// WebhookRawRejected 签名校验失败的 webhook 仅落 audit 表，不占 webhook_raw
// 的 (adapter, event_id, event_type) UNIQUE —— 否则攻击者可以用伪造 event_id
// 的 bad-signature 请求把合法 event_id 的 dedupe slot 抢占掉。
type WebhookRawRejected struct {
	ID         uint64
	PiID       string
	Adapter    string
	EventID    string
	EventType  string
	Reason     string // signature_fail / amount_mismatch / replay 等
	Headers    string // raw JSON
	Body       string
	ReceivedAt time.Time
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
	ErrIdempotentHit        = errors.New("idempotent hit: returning prior response")
	ErrAcquirerTxNotFound   = errors.New("acquirer_tx not found")
	ErrWebhookAlreadySeen   = errors.New("webhook already seen (dedupe hit)")
	ErrChannelUnavailable   = errors.New("channel unavailable")
	ErrChannelSignatureFail = errors.New("channel signature verification failed")
	// ErrWebhookRateLimited per-adapter 限流命中。HTTP 层应回 429。
	ErrWebhookRateLimited = errors.New("webhook rate limited")
	// ErrWebhookReplay payload 时间戳超出 replay window。HTTP 层应回 400 / 401。
	ErrWebhookReplay = errors.New("webhook timestamp outside replay window")
	// ErrWebhookAmountMismatch webhook 金额/币种与本地 acquirer_tx 不一致。
	// 强烈怀疑是恶意伪造或上游 bug，必须拒绝转发并告警。
	ErrWebhookAmountMismatch = errors.New("webhook amount/currency does not match local acquirer_tx")
	// ErrUnsupported 渠道不支持该操作（如 Maya/PayMongo/GCash 没有独立 Capture/Void）。
	// 上层不能把它当成「成功」处理。
	ErrUnsupported = errors.New("operation not supported by this channel")
)
