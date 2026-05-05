// middleware.go: HTTP middleware - card-center HTTPS 入口的 3 道身份防线。
//
//   ┌─────────────────────────────────────────────────────────────────────┐
//   │ 防线 1: Session 认证                                                  │
//   │   抽 jwt（Authorization: Bearer 或 Cookie uauth）；空 → 401          │
//   ├─────────────────────────────────────────────────────────────────────┤
//   │ 防线 2: 用户登录态校验                                                │
//   │   Verifier.Verify(jwt) → 走 mTLS gRPC 调 user-merchant-core           │
//   │   IntrospectToken；返回 valid=false 或 RPC 失败 → 401                │
//   ├─────────────────────────────────────────────────────────────────────┤
//   │ 防线 3: 是否是登录本人 (self-only)                                    │
//   │   user_id **只**从 jwt 解出，注入 ctx；任何 handler 必须用 ctx 取，   │
//   │   不允许 body / query / form 提供 user_id（即便提供也立即拒，不仅   │
//   │   是 "校验匹配后放行"）。任何针对卡的写操作（删卡/设默认）在 SQL    │
//   │   层 WHERE user_id=ctx.user_id 二次过滤。                             │
//   └─────────────────────────────────────────────────────────────────────┘
//
// handler 内部通过 UserIDFromCtx(ctx) 拿 user_id；body / query / form 里的
// user_id 参数被 RejectClaimedUserID 直接拒绝，而不是 "如果匹配就放行"。
package httpsauth

import (
	"context"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// ctxKey context key 类型（unexported 避免冲突）
type ctxKey int

const (
	ctxKeyUserID ctxKey = iota + 1
	ctxKeyJWTHash
)

// CookieName 跟 api-gateway userweb.CookieName 保持一致
const CookieName = "uauth"

// Middleware 校验 jwt → 注入 user_id；失败 401。
//
// jwt 来源（按优先级）：
//  1. Authorization: Bearer <jwt>
//  2. Cookie: uauth=<jwt>
func Middleware(verifier Verifier, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jwt := extractJWT(r)
			if jwt == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "unauthorized: jwt required (Authorization: Bearer or cookie uauth)",
				})
				return
			}
			uid, valid, err := verifier.Verify(r.Context(), jwt)
			if err != nil || !valid {
				logger.Debug("https auth failed", zap.Error(err), zap.Bool("valid", valid))
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "unauthorized: invalid or expired jwt",
				})
				return
			}
			// 注入 user_id 到 ctx
			ctx := context.WithValue(r.Context(), ctxKeyUserID, uid)
			ctx = context.WithValue(ctx, ctxKeyJWTHash, sha256Hex(jwt)[:16])
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserIDFromCtx 从 ctx 取 user_id；handler 必须用这个，不能从 request body 读。
func UserIDFromCtx(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxKeyUserID).(int64)
	if !ok || v == 0 {
		return 0, false
	}
	return v, true
}

// JWTHashFromCtx 取 jwt 的短摘要（用作 audit 关联）；明文 jwt 永远不进 ctx。
func JWTHashFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyJWTHash).(string); ok {
		return v
	}
	return ""
}

// extractJWT 按优先级抽 jwt
func extractJWT(r *http.Request) string {
	// 1. Authorization: Bearer
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	// 2. cookie
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value
	}
	return ""
}

// AssertSelfUserID 防越权：handler 在拿到一个 input.user_id 时，必须跟 ctx user_id 比对。
//
// 大部分 handler 应该直接从 ctx 取 user_id 用，而不是从 input 拿。这个函数是给
// 必须显式传 user_id 的极少数场景（如 admin 工具，但 admin 不该走 HTTPS REST）兜底。
func AssertSelfUserID(ctx context.Context, claimed int64) error {
	authed, ok := UserIDFromCtx(ctx)
	if !ok {
		return ErrInvalidJWT
	}
	if claimed != 0 && claimed != authed {
		return ErrCrossUserAccess
	}
	return nil
}

// RejectClaimedUserID 比 AssertSelfUserID 更严：**任何**非零 user_id 都拒，
// 不管是否跟 ctx 匹配。
//
// 用法：handler 从 body decode 出 input 后立即调本函数。
//
//	if err := RejectClaimedUserID(input.UserID); err != nil { ... 403 ... }
//
// 理由：让"客户端永远不要发 user_id"成为协议约束。即便发了匹配的，也是错的客户端
// 实现，应该明确报 403 让前端开发者修。这避免了诸如"前端缓存了上次的 user_id 误发"
// 之类的边角问题，也让 audit_log 一眼看出客户端有没有按规约写。
func RejectClaimedUserID(claimed int64) error {
	if claimed != 0 {
		return ErrCrossUserAccess
	}
	return nil
}
