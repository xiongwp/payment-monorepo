// scope.go — scope 层级 + 通配符匹配。
//
// 普通业务用 mw.HasScope(ctx, "refund:write") 严格匹配即可 (定义在 oauth_bearer.go)。
// 这里加层级是 advanced 用法:
//
//   "refund:*"        → 包含 refund:read, refund:write, refund:cancel ...
//   "*:read"          → 全部 read 类
//   "*:*" / "*"       → 所有 (root, 慎用)
//
// 顶层 "<resource>:write" 隐含 "<resource>:read" — 给 ops/super-admin 用。
//
// 用法:
//   if mw.HasScopeOrImplied(ctx, "refund:read") { ... }
//     ↑ 有 "refund:write" / "refund:*" / "*:read" / "*:*" 任一个就算

package mw

import (
	"context"
	"net/http"
	"strings"
)

// HasScopeOrImplied 检查 actor 是否有 scope (含层级 + 通配)。
func HasScopeOrImplied(ctx context.Context, want string) bool {
	a := ActorFromCtx(ctx)
	for _, granted := range a.Scopes {
		if scopeMatches(granted, want) {
			return true
		}
	}
	return false
}

// scopeMatches granted 是否覆盖 want。
//
// 规则:
//   granted=want                 ✓ 严格匹配
//   granted="*" 或 "*:*"          ✓ 全包
//   granted="resource:*"         ✓ 覆盖 "resource:<anything>"
//   granted="*:action"           ✓ 覆盖 "<anything>:action"
//   granted="resource:write"     ✓ 隐含覆盖 "resource:read"
func scopeMatches(granted, want string) bool {
	if granted == want {
		return true
	}
	if granted == "*" || granted == "*:*" {
		return true
	}
	gParts := strings.SplitN(granted, ":", 2)
	wParts := strings.SplitN(want, ":", 2)
	if len(gParts) != 2 || len(wParts) != 2 {
		return false
	}
	resMatch := gParts[0] == "*" || gParts[0] == wParts[0]
	actMatch := gParts[1] == "*" || gParts[1] == wParts[1]
	if resMatch && actMatch {
		return true
	}
	// write 隐含 read (同 resource)
	if resMatch && gParts[1] == "write" && wParts[1] == "read" {
		return true
	}
	return false
}

// RequireScopeOrImplied 中间件版本，支持层级。
func RequireScopeOrImplied(want string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !HasScopeOrImplied(r.Context(), want) {
				http.Error(w, "forbidden: missing scope "+want, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AnyScope 任一 scope 命中即放行 (隐含 + 通配)。
func AnyScope(wants ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, want := range wants {
				if HasScopeOrImplied(r.Context(), want) {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "forbidden: need one of "+strings.Join(wants, ","), http.StatusForbidden)
		})
	}
}

// AllScopes 全部 scope 都命中才放行。
func AllScopes(wants ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, want := range wants {
				if !HasScopeOrImplied(r.Context(), want) {
					http.Error(w, "forbidden: missing scope "+want, http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
