// Package auth — OIDC + MFA HTTP middleware。
//
// 调用链：
//
//	browser → admin-web (Workbench) → POST /admin/* with
//	  Authorization: Bearer <id_token>
//	  X-Risk-MFA-Code: 123456    (admin 类写操作必带)
//	→ OIDCMiddleware:
//	    1) 解 Bearer → OIDCProvider.Verify → Claims
//	    2) 路径匹配 mfaRequiredPaths → 校验 X-Risk-MFA-Code via TOTPVerifier
//	    3) 注入 *Principal{KeyID=sub, AdminRole=fromGroups(claims.Groups)} 进 ctx
//	    4) 下游 handler 用 ActorFromContext(ctx) 拿真实 actor，不再信 r.FormValue("actor")
//	→ 401 失败时返 WWW-Authenticate: Bearer error="..." 提示前端重 SSO 流程
//
// Feature flag 切换：cfg.admin.oidc.enabled = true → 路由用本 middleware，否则
// 继续走 metrics.AdminAuthRoles 老逻辑（向后兼容）。本文件不依赖 commercial.go。
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ContextKey for principal injected by OIDCMiddleware。复用同包 ctxKey{}，
// 保证 PrincipalFrom / WithPrincipal 跨 middleware 一致。

// MFAOptions 决定哪些请求要二步。默认空 → 全部 admin 路径都要。
type MFAOptions struct {
	// PathPrefixes 列出哪些路径需要 MFA（精确前缀匹配）。空 = 全部要。
	// 推荐至少：/admin/rules/、/admin/review/decide、/admin/ml/override 这类写操作。
	PathPrefixes []string
	// SafeMethods GET/HEAD/OPTIONS 默认不要 MFA（read-only）。设 true 表示连读也要。
	StrictReads bool
}

// OIDCMiddleware Bearer ID Token + optional MFA + 注入 Principal。
//
// provider == nil → middleware 完全放行（dev / OIDC 未启用）。生产应 enabled = true
// 让 fx 注入非 nil provider，main.go 选用 OIDCMiddleware 替换 AdminAuthRoles。
//
// mfa == nil 等价 MFA 关闭（OIDC-only，CC6.1 不达标但调试期可用）。
func OIDCMiddleware(provider *OIDCProvider, mfa *TOTPVerifier, opts MFAOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if provider == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, err := extractBearer(r)
			if err != nil {
				oidcFail(w, "invalid_token", err.Error())
				return
			}
			claims, err := provider.Verify(r.Context(), tok)
			if err != nil {
				oidcFail(w, "invalid_token", err.Error())
				return
			}
			// MFA 检查
			if mfa != nil && needsMFA(r, opts) {
				code := r.Header.Get("X-Risk-MFA-Code")
				if code == "" {
					oidcFail(w, "mfa_required", "X-Risk-MFA-Code header missing")
					return
				}
				if !mfa.Verify(claims.Subject, code) {
					oidcFail(w, "mfa_invalid", "totp verify failed")
					return
				}
			}
			p := &Principal{
				Scope:     ScopeAdmin,
				KeyID:     claims.Subject,
				AdminRole: roleFromGroups(claims.Groups),
			}
			ctx := WithPrincipal(r.Context(), p)
			// 把 email / name 也带上方便 audit 拿；走另一个 ctx key。
			ctx = withActor(ctx, &ActorInfo{
				UserID: claims.Subject,
				Email:  claims.Email,
				Name:   claims.Name,
				Groups: claims.Groups,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractBearer(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", errors.New("missing bearer")
	}
	return strings.TrimSpace(h[len(p):]), nil
}

func needsMFA(r *http.Request, opts MFAOptions) bool {
	if !opts.StrictReads {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return false
		}
	}
	if len(opts.PathPrefixes) == 0 {
		return true
	}
	for _, pfx := range opts.PathPrefixes {
		if strings.HasPrefix(r.URL.Path, pfx) {
			return true
		}
	}
	return false
}

// roleFromGroups 把 IdP group claim 映射到 AdminRole。约定：
//   - "risk-admin" / "danger"   → danger（rule edit / ML override）
//   - "risk-write" / "write"    → write （review decide / outcome）
//   - "risk-read"  / "read"     → read  （只读看板）
//   - 其他 / 无 group           → read（最小权限）
func roleFromGroups(groups []string) string {
	rank := 0
	for _, g := range groups {
		switch strings.ToLower(g) {
		case "risk-admin", "danger":
			if rank < 3 {
				rank = 3
			}
		case "risk-write", "write":
			if rank < 2 {
				rank = 2
			}
		case "risk-read", "read":
			if rank < 1 {
				rank = 1
			}
		}
	}
	switch rank {
	case 3:
		return "danger"
	case 2:
		return "write"
	default:
		return "read"
	}
}

func oidcFail(w http.ResponseWriter, errCode, desc string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="`+errCode+`", error_description="`+strings.ReplaceAll(desc, `"`, `'`)+`"`)
	status := http.StatusUnauthorized
	if errCode == "mfa_required" || errCode == "mfa_invalid" {
		// 403 + error code 让前端区分 "需要 MFA" vs "SSO 完全失败"
		status = http.StatusForbidden
	}
	http.Error(w, `{"error":"`+errCode+`"}`, status)
}

// ── Actor (ctx-carried admin identity) ───────────────────────────────────
// Principal 抓 KeyID + AdminRole 做权限决策；ActorInfo 给 audit log 写人话
// （email / display name），review handler 用 ActorFromContext 拿，禁止
// 信任入站 r.FormValue("actor") / r.Header.Get("X-Actor")。

// ActorInfo audit 用的人类可读身份。
type ActorInfo struct {
	UserID string
	Email  string
	Name   string
	Groups []string
}

// ActorString 返一个 audit-friendly string："email | sub"。
func (a *ActorInfo) ActorString() string {
	if a == nil {
		return ""
	}
	if a.Email != "" {
		return a.Email
	}
	return a.UserID
}

type actorCtxKey struct{}

func withActor(ctx context.Context, a *ActorInfo) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// ActorFromContext 从 ctx 拿 OIDC 注入的 actor。OIDC 未启用 / dev 模式 → nil。
//
// review handler 用法：
//
//	a := auth.ActorFromContext(r.Context())
//	if a == nil { http.Error(w, "no actor", 403); return }
//	actor := a.ActorString()
func ActorFromContext(ctx context.Context) *ActorInfo {
	a, _ := ctx.Value(actorCtxKey{}).(*ActorInfo)
	return a
}
