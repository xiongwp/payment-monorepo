// attempt.go — 每次投递尝试的不可变历史.
//
// 跟 Delivery (status mutable, 表示当前) 不同, Attempt 是 append-only audit log.
//
// 商户 / ops 用途:
//   - GET /v1/me/deliveries/{id}/attempts — 商户看每次重试 status code + 响应体
//   - 调 DLQ 排查: 哪次开始挂 → 看响应 body 知道商户 502 还是 timeout
//   - 合规审计: 商户没收到的责任界定 (我们尝试了 N 次都 5xx → 我们已 best effort)

package domain

import "time"

// Attempt 单次投递尝试快照. Delivery 每 retry 一次 append 一条.
type Attempt struct {
	ID            int64     `db:"id" json:"id"`
	DeliveryID    int64     `db:"delivery_id" json:"delivery_id"`
	AttemptNum    int       `db:"attempt_num" json:"attempt_num"`     // 第几次重试 (1-based)
	HTTPStatus    int       `db:"http_status" json:"http_status,omitempty"`
	DurationMS    int       `db:"duration_ms" json:"duration_ms"`
	ErrorMessage  string    `db:"error_message" json:"error_message,omitempty"`
	ResponseBody  string    `db:"response_body" json:"response_body,omitempty"` // 截 2KB
	SignatureTS   int64     `db:"signature_ts" json:"signature_ts"`   // 当时签名用的 t=
	RequestSent   bool      `db:"request_sent" json:"request_sent"`   // false = 还没连上对方 (DNS/conn refused)
	SecretKeyID   string    `db:"secret_key_id" json:"secret_key_id,omitempty"` // 当时用的 key id (primary/secondary)
	OccurredAt    time.Time `db:"occurred_at" json:"occurred_at"`
}
