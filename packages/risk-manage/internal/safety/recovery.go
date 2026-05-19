// Package safety 稳定性 + 安全性中间件集合：
//
//   - panic recovery for HTTP / gRPC （主路径任一 panic 不能拖死服务）
//   - PII 脱敏 zap field encoder + 单字符串 helper
//   - pprof endpoint guard (prod 不应暴露在公网；admin path 内才允许)
//
// 设计哲学：所有中间件 fail-open（panic 时返 503 / 5xx 错误而不是阻塞 +
// 让监控指标看到），不影响合法路径。
package safety

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"go.uber.org/zap"
)

// HTTPRecovery panic recovery middleware：捕获所有 handler panic，log stack
// trace + 返 500 不让进程死。生产 admin / metrics HTTP server 都应该套一层。
func HTTPRecovery(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// 完整 stack trace 落 log；返给 caller 的只有 generic
					// 错误，避免泄漏内部细节。
					if logger != nil {
						logger.Error("HTTP handler panic recovered",
							zap.Any("recover", rec),
							zap.String("method", r.Method),
							zap.String("path", r.URL.Path),
							zap.ByteString("stack", debug.Stack()))
					}
					http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// GRPCUnaryRecovery 已删 — gRPC interceptor 不适用 Kitex (0 caller).
// 等价 Kitex middleware 走 kitexutil.RecoverMW.

// PathScopedPprof 仅在 prefix-matched 路径下暴露 net/http/pprof handlers。
// 生产部署必须挂在 admin 端口（带 token auth），千万不要暴露公网（CPU 吃满
// + heap dump 泄漏数据）。
//
// 用法：
//
//	mux.Handle("/admin/debug/pprof/", PathScopedPprof("/admin/debug/pprof"))
//
// 然后 curl /admin/debug/pprof/profile?seconds=30 拿 CPU profile。
func PathScopedPprof(_ string) http.Handler {
	// net/http/pprof package 在 init 时把 handler 注册到 DefaultServeMux 的
	// /debug/pprof/* 路径。我们把那些 handler "重路由" 到自定义 prefix。
	mux := http.NewServeMux()
	// 这里直接 forward 到 DefaultServeMux 的 /debug/pprof/*
	// （需要 import _ "net/http/pprof" — 见 main.go）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 把 r.URL.Path 改写成 /debug/pprof/... 让 DefaultServeMux 找到 handler
		// 例如 /admin/debug/pprof/heap → /debug/pprof/heap
		path := r.URL.Path
		// 找到 "pprof" 之后的部分
		idx := indexOf(path, "pprof")
		if idx < 0 {
			http.NotFound(w, r)
			return
		}
		newPath := "/debug/pprof" + path[idx+len("pprof"):]
		r.URL.Path = newPath
		http.DefaultServeMux.ServeHTTP(w, r)
	})
	return mux
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// MustPanic helper for testing: 直接 panic 让 recovery 中间件吃掉。
func MustPanic(msg string) {
	panic(fmt.Errorf("test panic: %s", msg))
}
