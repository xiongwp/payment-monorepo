// Package httpsauth 提供 card-center HTTPS 入口的认证辅助。
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  浏览器                                                                   │
// │   │                                                                      │
// │   ▼ HTTPS + cookie(uauth=jwt) 或 Authorization: Bearer <jwt>             │
// │  ┌─────────────────┐                                                     │
// │  │ card-center     │                                                     │
// │  │  HTTPS REST     │                                                     │
// │  │   middleware    │                                                     │
// │  └────────┬────────┘                                                     │
// │           │ mTLS gRPC IntrospectToken(jwt)                               │
// │           ▼                                                              │
// │  ┌─────────────────┐                                                     │
// │  │ user-merchant-  │ → 验签 + session 状态 + 返 { valid, user_id }       │
// │  │ core.UserService│                                                     │
// │  └─────────────────┘                                                     │
// └─────────────────────────────────────────────────────────────────────────┘
//
// 设计原则：
//   - card-center 自己不解 JWT、不存任何用户身份信息
//   - 用户登录态权威 = user-merchant-core；mTLS gRPC 走 internal listener
//   - card-center 客户端 cert CN = "card-center"，user-merchant-core 在 internal
//     listener 上 clientCN 白名单含 "card-center"
//   - 60s 短期缓存 user_id 减少 RPC 调用（trade-off：登出后最多 60s 仍能用旧 jwt）
//
// 安全纪律：
//   - 任何请求必须有合法 jwt，否则 401
//   - 请求里**不接受** user_id 参数；server-side 用 verify 出来的 user_id
//   - user_id 一旦从 jwt 解出，就锁死到 ctx，handler 用 ctx 取，绝不读 form/query/header
package httpsauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Verifier 抽象用户登录态校验。
//
// 当前唯一生产实现：UserMerchantVerifier（verifier_user_merchant.go），通过
// mTLS gRPC 调 user-merchant-core.IntrospectToken。
//
// 接口保留是为了：
//   1. 单测：fakeVerifier 直接返预设 user_id
//   2. 未来可能的二级缓存 / 风控前置（包一层 decorator 即可）
type Verifier interface {
	Verify(ctx context.Context, jwt string) (userID int64, valid bool, err error)
}

// 错误集
var (
	// ErrInvalidJWT JWT 不合法 / 过期
	ErrInvalidJWT = errors.New("httpsauth: jwt invalid or expired")
	// ErrCrossUserAccess 防越权：请求里 claimed user_id 跟 ctx user_id 不一致
	ErrCrossUserAccess = errors.New("httpsauth: cross-user access denied")
)

// sha256Hex 共用 helper：sha256 → hex
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// parseInt64 共用 helper：纯数字字符串 → int64（不依赖 strconv 减少 import）
func parseInt64(s string) (int64, error) {
	if len(s) == 0 {
		return 0, errors.New("empty")
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q at %d", c, i)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
