// Package domain — 商户出站 webhook 事件 + endpoint 配置实体。
//
// 设计目标：
//   1. 商户每个事件可订/可不订（endpoint × event_type 多对多）
//   2. 签名 HMAC-SHA256(secret, body)，header `X-Webhook-Signature`
//   3. 重试策略：3 天指数退避（10 次重试间隔 1m / 5m / 15m / 1h / 4h / 12h / 24h / 24h / 24h / 24h）
//   4. 死信：3 天后 / 10 次重试还失败 → 进 DLQ + 商户后台高亮 + ops alert
//   5. 顺序保证：同 endpoint 单线程串行（按 created_at），防止商户收到乱序事件
//   6. 至少一次：商户必须幂等（payload 含 event_id；商户 dedupe）
//   7. Replay：ops 后台手动重发某事件（合规审计 / 调试用）

package domain

import "time"

// Endpoint 商户配置的 webhook endpoint。
type Endpoint struct {
	ID            int64     `db:"id" json:"id"`
	MerchantID    string    `db:"merchant_id" json:"merchant_id"`
	URL           string    `db:"url" json:"url"`
	SecretEnc     string    `db:"secret_enc" json:"-"`               // KMS 加密
	SecretLast4   string    `db:"secret_last4" json:"secret_last4"`  // 给 UI 显示
	EventTypes    string    `db:"event_types" json:"event_types"`    // CSV: "charge.succeeded,refund.created"
	Description   string    `db:"description" json:"description,omitempty"`
	Active        bool      `db:"active" json:"active"`
	TLSVerify     bool      `db:"tls_verify" json:"tls_verify"`      // 默认 true；测试环境可关
	MaxRetries    int       `db:"max_retries" json:"max_retries"`    // 默认 10
	TimeoutMS     int       `db:"timeout_ms" json:"timeout_ms"`      // 默认 10000 (10s)
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time `db:"updated_at" json:"updated_at"`
	LastDeliveryAt *time.Time `db:"last_delivery_at" json:"last_delivery_at,omitempty"`
}

// Event 一条待发或已发的 webhook 事件。
type Event struct {
	ID          int64       `db:"id" json:"id"`
	MerchantID  string      `db:"merchant_id" json:"merchant_id"`
	EventType   string      `db:"event_type" json:"event_type"`   // charge.succeeded / refund.created / dispute.received / ...
	EventID     string      `db:"event_id" json:"event_id"`       // 业务事件唯一 ID (evt_xxx) — 商户用来 dedupe
	Payload     []byte      `db:"payload" json:"payload"`         // JSON body
	APIVersion  string      `db:"api_version" json:"api_version"` // "2026-05-01"
	TraceID     string      `db:"trace_id" json:"trace_id,omitempty"`
	CreatedAt   time.Time   `db:"created_at" json:"created_at"`
}

// Delivery 一次具体的投递尝试（一个 event × 一个 endpoint × N 次重试 = N 条 Delivery）。
type Delivery struct {
	ID              int64           `db:"id" json:"id"`
	EventID         int64           `db:"event_id" json:"event_id"`
	EndpointID      int64           `db:"endpoint_id" json:"endpoint_id"`
	Attempt         int             `db:"attempt" json:"attempt"`           // 1-based; 第几次重试
	Status          DeliveryStatus  `db:"status" json:"status"`
	HTTPStatus      int             `db:"http_status" json:"http_status,omitempty"`
	ResponseBody    string          `db:"response_body" json:"response_body,omitempty"` // 截断到 2KB
	ErrorMessage    string          `db:"error_message" json:"error_message,omitempty"`
	DurationMS      int             `db:"duration_ms" json:"duration_ms"`
	SentAt          *time.Time      `db:"sent_at" json:"sent_at,omitempty"`
	NextRetryAt     *time.Time      `db:"next_retry_at" json:"next_retry_at,omitempty"`
	CreatedAt       time.Time       `db:"created_at" json:"created_at"`
}

// DeliveryStatus 投递状态。
type DeliveryStatus string

const (
	StatusPending   DeliveryStatus = "pending"
	StatusInflight  DeliveryStatus = "inflight"
	StatusDelivered DeliveryStatus = "delivered" // 2xx 收到
	StatusFailed    DeliveryStatus = "failed"    // 非 2xx 或网络错；将进重试
	StatusDLQ       DeliveryStatus = "dlq"       // 达 max_retries 后归档
)

// IsTerminal 终态判定。
func (s DeliveryStatus) IsTerminal() bool {
	return s == StatusDelivered || s == StatusDLQ
}

// RetryBackoff 指数退避: 1m, 5m, 15m, 1h, 4h, 12h, 24h, 24h, 24h, 24h。
//
// attempt 是已经失败的次数（1 = 第一次刚失败，等下次重试间隔）。
func RetryBackoff(attempt int) time.Duration {
	schedule := []time.Duration{
		1 * time.Minute, 5 * time.Minute, 15 * time.Minute,
		1 * time.Hour, 4 * time.Hour, 12 * time.Hour,
		24 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour,
	}
	if attempt <= 0 {
		return schedule[0]
	}
	if attempt > len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[attempt-1]
}
