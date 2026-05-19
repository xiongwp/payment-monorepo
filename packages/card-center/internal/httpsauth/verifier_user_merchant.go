// verifier_user_merchant.go: 唯一生产 Verifier 实现 — mTLS gRPC 直连 user-merchant-core.IntrospectToken。
//
// 在 card-center HTTPS 入口的 3 道防线里，本组件**实现防线 2**（用户登录态校验）：
//
//	防线 1 (middleware.extractJWT): 浏览器必须带合法 Authorization 或 Cookie
//	防线 2 (本文件 Verifier.Verify): jwt 必须能在 user-merchant-core 通过 IntrospectToken
//	                                  → 同时拿到权威 user_id
//	防线 3 (handler RejectClaimedUserID): user_id 只从 jwt，body 不许带
//
// 调用路径：
//
//	浏览器 cookie/Bearer jwt → card-center HTTPS → user-merchant-core
//	   IntrospectToken (mTLS gRPC, internal listener) → 返 user_id
//
// 选这条路而不是经 api-gateway 的原因：
//   - 一跳；user-merchant-core 本来就是登录态权威
//   - api-gateway 是公网边界，让它再做内部认证代理是不必要的耦合
//   - mTLS 客户端 cert CN = "card-center"，user-merchant-core 在 internal listener
//     上只白名单含 "card-center" 的 CN，跨服务调用边界清晰
package httpsauth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1"
)

// cacheEntry 短期缓存 jwt → user_id 校验结果
type cacheEntry struct {
	userID    int64
	valid     bool
	expiresAt time.Time
}

// UserMerchantVerifier 通过 mTLS gRPC 调 user-merchant-core.UserService.IntrospectToken。
//
// 60s 短期缓存避免每个 HTTPS 请求都打 RPC（每秒可能数百绑卡 / 列卡请求）。
// 登出后最多 60s 仍能用旧 jwt — 业务可接受；不可接受时把 cacheTTL 调到 0 即可。
type UserMerchantVerifier struct {
	uc       userservice.Client
	timeout  time.Duration
	cacheTTL time.Duration
	cache    sync.Map // map[sha256(jwt)]cacheEntry
	logger   *zap.Logger
}

// NewUserMerchantVerifier 构造
func NewUserMerchantVerifier(uc userservice.Client, logger *zap.Logger) *UserMerchantVerifier {
	return &UserMerchantVerifier{
		uc:       uc,
		timeout:  3 * time.Second,
		cacheTTL: 60 * time.Second,
		logger:   logger,
	}
}

// Verify 校验 jwt 合法性，返回 user_id。
func (v *UserMerchantVerifier) Verify(parent context.Context, jwt string) (int64, bool, error) {
	if jwt == "" {
		return 0, false, ErrInvalidJWT
	}
	key := sha256Hex(jwt)
	now := time.Now()
	if c, ok := v.cache.Load(key); ok {
		ce := c.(cacheEntry)
		if now.Before(ce.expiresAt) {
			return ce.userID, ce.valid, nil
		}
		v.cache.Delete(key)
	}

	ctx, cancel := context.WithTimeout(parent, v.timeout)
	defer cancel()
	resp, err := v.uc.IntrospectToken(ctx, &usermerchantv1.IntrospectTokenRequest{Jwt: jwt})
	if err != nil {
		v.logger.Debug("IntrospectToken rpc error", zap.Error(err))
		return 0, false, fmt.Errorf("user-merchant-core IntrospectToken: %w", err)
	}
	if !resp.GetValid() {
		return 0, false, ErrInvalidJWT
	}
	uid, err := parseInt64(resp.GetUserId())
	if err != nil {
		return 0, false, fmt.Errorf("bad user_id from user-merchant-core: %w", err)
	}
	// 仅缓存成功结果（避免短时间错误 → 缓存投毒）
	v.cache.Store(key, cacheEntry{userID: uid, valid: true, expiresAt: now.Add(v.cacheTTL)})
	return uid, true, nil
}
