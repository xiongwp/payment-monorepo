// middleware.go: HTTP middleware - 抽 jwt → Verifier 校验 → 注入 user_id 到 ctx。
//
// 任何 card-center HTTPS handler 都应该走这个 middleware；handler 内部通过
// UserIDFromCtx(ctx) 拿 user_id，**不接受**请求 body / query 里的 user_id 参数。
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
