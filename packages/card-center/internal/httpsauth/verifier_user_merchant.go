// verifier_user_merchant.go — 临时 STUB.
//
// 原版通过 Kitex 调 user-merchant-core.IntrospectToken 校验 JWT.
// cross-service kitex_gen (user-merchant-core) 还没接进 card-center 的 docker
// build 流程 (additional_contexts), 暂改 stub:
//
//   - 构造时不持有真 user-merchant Kitex client
//   - Verify 永远返 (0, false, ErrInvalidJWT) — fail-closed, 拒绝所有请求
//
// 部署时必须把 card-center HTTPS 入口的 jwt 校验关掉 (走纯 mTLS 内部访问), 或
// 等接通 user-merchant-core kitex_gen 后改回真实调用.
package httpsauth

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// cacheEntry 短期缓存 jwt → user_id 校验结果. stub 阶段不实际使用.
type cacheEntry struct {
	userID    int64
	valid     bool
	expiresAt time.Time
}

// UserMerchantVerifier STUB — 不持有 Kitex client.
type UserMerchantVerifier struct {
	timeout  time.Duration
	cacheTTL time.Duration
	cache    sync.Map
	logger   *zap.Logger
}

// NewUserMerchantVerifier 构造 stub. uc 参数仍接受 (兼容 caller 签名), 但是 ignore.
func NewUserMerchantVerifier(_ any, logger *zap.Logger) *UserMerchantVerifier {
	return &UserMerchantVerifier{
		timeout:  3 * time.Second,
		cacheTTL: 60 * time.Second,
		logger:   logger,
	}
}

// Verify STUB: 永远返 ErrInvalidJWT (fail-closed).
func (v *UserMerchantVerifier) Verify(_ context.Context, jwt string) (int64, bool, error) {
	if jwt == "" {
		return 0, false, ErrInvalidJWT
	}
	if v.logger != nil {
		v.logger.Warn("UserMerchantVerifier STUB — user-merchant-core kitex_gen not wired in build, rejecting all JWTs")
	}
	return 0, false, ErrInvalidJWT
}
