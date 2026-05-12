// sandbox_mode.go — Test mode (sandbox) vs live (生产) 隔离守护.
//
// oauth2-server 颁的 client_id 带 mode 前缀 (pk_test_ / pk_live_).
// payment-mw 在 OAuth bearer 中间件后挂这个守护, 防 test key 调到 live 资源.
//
// 规则:
//   - test key 调 /v1/* 业务 API: 路由到 sandbox env (实际跑 mock 服务)
//   - live key 调 /v1/* : 走生产 service
//   - test key 永远不能 access live 数据 (即使 scope 允许; mode 是更强的约束)
//
// 实施:
//   - 解 client_id 前缀 → mode
//   - 把 mode 写入 ctx, downstream handler 用 ModeFromCtx 取
//   - 通过 IsTestMode 判断是否要去 *-mock backend

package paymentmw

import (
	"context"
	"net/http"
	"strings"
)

// Mode test (sandbox) | live (prod)
type Mode string

const (
	ModeTest    Mode = "test"
	ModeLive    Mode = "live"
	ModeUnknown Mode = ""
)

// ctxKey 防与 strings 冲突
type modeCtxKey struct{}

// ParseMode 从 client_id 反推 mode.
func ParseMode(clientID string) Mode {
	switch {
	case strings.HasPrefix(clientID, "pk_test_"),
		strings.HasPrefix(clientID, "svc_test_"),
		strings.HasPrefix(clientID, "ops_test_"):
		return ModeTest
	case strings.HasPrefix(clientID, "pk_live_"),
		strings.HasPrefix(clientID, "svc_live_"),
		strings.HasPrefix(clientID, "ops_live_"):
		return ModeLive
	}
	// 老 client_id (没前缀) — 兜底当 live (向后兼容)
	return ModeLive
}

// WithMode 把 mode 写入 ctx (oauth_bearer.go 解出 token 后调).
func WithMode(ctx context.Context, m Mode) context.Context {
	return context.WithValue(ctx, modeCtxKey{}, m)
}

// ModeFromCtx 取 mode. 没有则 unknown.
func ModeFromCtx(ctx context.Context) Mode {
	if m, ok := ctx.Value(modeCtxKey{}).(Mode); ok {
		return m
	}
	return ModeUnknown
}

// IsTest 快捷方法
func IsTest(ctx context.Context) bool {
	return ModeFromCtx(ctx) == ModeTest
}

// EnforceLiveOnly 中间件: 阻止 test key 调 live-only 路由.
//
// 用于 /admin/* 和明确不允许 sandbox 的接口 (e.g. 真出款 / 真税表 e-file).
func EnforceLiveOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ModeFromCtx(r.Context()) == ModeTest {
			http.Error(w, `{"error":"test_key_forbidden","message":"This endpoint requires a live key. Test keys cannot access this resource."}`,
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RouteByMode 把 test 流量打到 -mock backend, live 走生产.
// 调用方传两个 base URL; 中间件根据 ctx mode 选一个.
type RouteByModeOpt struct {
	LiveBaseURL string
	TestBaseURL string // 一般是 *-mock service URL
}

// SelectBackend 根据 ctx 返合适的 base URL.
func SelectBackend(ctx context.Context, opt RouteByModeOpt) string {
	if ModeFromCtx(ctx) == ModeTest && opt.TestBaseURL != "" {
		return opt.TestBaseURL
	}
	return opt.LiveBaseURL
}
