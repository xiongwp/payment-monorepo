package channel

import (
	"context"
	"time"
)

// ThreeDSChallengeRequest 发起 3DS 挑战的入参
type ThreeDSChallengeRequest struct {
	PaymentIntentID string
	ChargeID        string
	Amount          int64
	Currency        string
	// PaymentMethodRef 发卡行识别所需信息（token / 卡号前 6 + 后 4 / network 等）
	PaymentMethodRef string
	// ReturnURL ACS 完成挑战后用户回跳本地的 URL（通常是 pi.return_url）
	ReturnURL string
	Extra     map[string]string
}

// ThreeDSChallenge 渠道返回的挑战
type ThreeDSChallenge struct {
	// RedirectURL 前端跳转去的 ACS 地址
	RedirectURL string
	// SessionID 服务端留存，用于 VerifyCallback 时反查下游
	SessionID string
	// ExpiresAt 过期时间；零值由服务层用默认 TTL 兜底
	ExpiresAt time.Time
}

// ThreeDSClient 3D Secure 对接接口。
//
// 真实对接（Adyen / Stripe / 本地 acquirer）实现此接口后注入到
// service.ThreeDSProvider 里即可；多个 acquirer 共存时可再加一层按卡 BIN 路由的分发器。
type ThreeDSClient interface {
	// CreateRedirect 向发卡行 / acquirer 发起 3DS v2 挑战
	CreateRedirect(ctx context.Context, req ThreeDSChallengeRequest) (*ThreeDSChallenge, error)
	// VerifyCallback 用 session_id + ACS 回调字段，校验挑战结果
	VerifyCallback(ctx context.Context, sessionID string, callback map[string]string) (ok bool, reason string, err error)
}
