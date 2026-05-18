// middleware.go — Kitex 服务端 / 客户端共用 middleware.
//
// Kitex 用 endpoint.Endpoint 链, 跟 grpc UnaryInterceptor 类似但签名不同.
// 这里抽象成 Middleware = func(next Endpoint) Endpoint, 业务代码用
// kitexutil.AuthMW("token") 注入到 server / client 即可.

package kitexutil

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Endpoint Kitex endpoint 抽象 (避免直接依赖 Kitex package 让 payment-util 不强绑版本).
// req / resp 实际是 *kitex.Args / *kitex.Result, 真实包装由 server.NewServer / client.NewClient 完成.
type Endpoint func(ctx context.Context, req, resp interface{}) error

// Middleware Endpoint 装饰器链.
type Middleware func(next Endpoint) Endpoint

// Chain 把多个 Middleware 按调用顺序串成单条链.
func Chain(mws ...Middleware) Middleware {
	return func(next Endpoint) Endpoint {
		for i := len(mws) - 1; i >= 0; i-- {
			next = mws[i](next)
		}
		return next
	}
}

// ─── Auth MW ───────────────────────────────────────────────────────────────

// AuthMW 校验 metadata "x-admin-token" — 跟 split-payment adminTokenInterceptor 等价.
//
// expectedToken 空时退化为 no-op (DEV 模式, 调用方自己 log warn 提醒).
//
// Kitex 在 server.NewServer 里通过 server.WithMiddleware(kitexutil.AuthMW(...)) 注册.
// req 里取 token 的方式跟 grpc.metadata 不同, 实际从 ctx 拿 (kitex 把 TTHeader 注入 ctx).
func AuthMW(expectedToken string) Middleware {
	if expectedToken == "" {
		return identity
	}
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			tok := tokenFromCtx(ctx)
			if tok != expectedToken {
				return fmt.Errorf("kitexutil: invalid or missing X-Admin-Token")
			}
			return next(ctx, req, resp)
		}
	}
}

// tokenFromCtx 从 Kitex ctx 提取 "x-admin-token" header.
//
// 占位实现 — 实际接 Kitex 时通过 metainfo.GetValue(ctx, "x-admin-token") 拿;
// 当前 payment-util 不直接 import kitex/metainfo, 业务代码生成 stub 时 cast 即可.
func tokenFromCtx(ctx context.Context) string {
	if v := ctx.Value(ctxKeyAdminToken); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

type ctxKey string

const ctxKeyAdminToken ctxKey = "x-admin-token"

// WithAdminToken 把 token 注入 ctx — Kitex client side 调用前用, 自动随 TTHeader 上传.
func WithAdminToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, ctxKeyAdminToken, token)
}

// ─── Log MW ────────────────────────────────────────────────────────────────

// LogMW access log — RPC 入口/出口 + duration + error.
//
// 跟 grpcsvc.AccessLogInterceptor 一致, 日志字段:
//
//	method=<rpc_name> caller=<peer> dur_ms=<ms> err=<error>
func LogMW(log *zap.Logger) Middleware {
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			start := time.Now()
			err := next(ctx, req, resp)
			elapsed := time.Since(start)
			if err != nil {
				log.Warn("kitex rpc error",
					zap.Duration("dur", elapsed),
					zap.Error(err))
			} else {
				log.Debug("kitex rpc ok",
					zap.Duration("dur", elapsed))
			}
			return err
		}
	}
}

// ─── Metrics MW ────────────────────────────────────────────────────────────

// MetricsMW RPC count + p50/p95 latency 推 Prometheus.
//
// 跟 grpcsvc.MetricsInterceptor 一致, 标签 (method, code). 当前 stub 实现, 真实接 Kitex
// 时换成 obsbootstrap 暴露的 prometheus.HistogramVec.
func MetricsMW() Middleware {
	var (
		mu    sync.Mutex
		count int64
		total time.Duration
	)
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			start := time.Now()
			err := next(ctx, req, resp)
			elapsed := time.Since(start)
			mu.Lock()
			count++
			total += elapsed
			mu.Unlock()
			return err
		}
	}
}

// ─── Recover MW ────────────────────────────────────────────────────────────

// RecoverMW panic recover → error.
//
// 跟 grpcsvc.PanicRecoverInterceptor 一致 — handler panic 时不挂进程, 转成 error
// 返给上游, 同时 zap.Error log + 栈.
func RecoverMW(log *zap.Logger) Middleware {
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			defer func() {
				if r := recover(); r != nil {
					log.Error("kitex rpc panic",
						zap.Any("panic", r),
						zap.ByteString("stack", debug.Stack()))
					err = fmt.Errorf("kitex: handler panic: %v", r)
				}
			}()
			return next(ctx, req, resp)
		}
	}
}

// ─── CircuitBreaker MW ─────────────────────────────────────────────────────

// CircuitBreakerMW per-RPC circuit breaker — 跟 observability.CircuitBreaker 等价.
//
// failureThreshold 连续失败 N 次 → open; openDuration 后半开试探一次; successThreshold
// 连续成功 N 次 → close 恢复.
//
// 当前 stub 实现, 真实接 Kitex 时可换成 circuitbreaker.CBSuite (Kitex 官方包).
type CircuitBreakerConfig struct {
	FailureThreshold int
	SuccessThreshold int
	OpenDuration     time.Duration
}

func CircuitBreakerMW(cfg CircuitBreakerConfig, log *zap.Logger) Middleware {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 2
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 30 * time.Second
	}
	var (
		state         int32 // 0 closed, 1 open, 2 half-open
		consecutiveOK int32
		consecutiveKO int32
		openedAt      int64 // unix nanos
	)
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			st := atomic.LoadInt32(&state)
			now := time.Now().UnixNano()
			if st == 1 {
				// open 状态 — 等开窗
				if time.Duration(now-atomic.LoadInt64(&openedAt)) < cfg.OpenDuration {
					return errors.New("kitexutil: circuit breaker OPEN")
				}
				atomic.StoreInt32(&state, 2) // half-open 试探
				log.Info("kitex circuit breaker half-open")
			}
			err := next(ctx, req, resp)
			if err != nil {
				atomic.StoreInt32(&consecutiveOK, 0)
				ko := atomic.AddInt32(&consecutiveKO, 1)
				if ko >= int32(cfg.FailureThreshold) && atomic.CompareAndSwapInt32(&state, 0, 1) {
					atomic.StoreInt64(&openedAt, time.Now().UnixNano())
					log.Warn("kitex circuit breaker OPENED",
						zap.Int32("consecutive_ko", ko))
				} else if ko >= int32(cfg.FailureThreshold) && atomic.LoadInt32(&state) == 2 {
					atomic.StoreInt32(&state, 1)
					atomic.StoreInt64(&openedAt, time.Now().UnixNano())
				}
				return err
			}
			atomic.StoreInt32(&consecutiveKO, 0)
			ok := atomic.AddInt32(&consecutiveOK, 1)
			if atomic.LoadInt32(&state) == 2 && ok >= int32(cfg.SuccessThreshold) {
				atomic.StoreInt32(&state, 0)
				log.Info("kitex circuit breaker CLOSED",
					zap.Int32("consecutive_ok", ok))
			}
			return nil
		}
	}
}

// identity no-op middleware (auth disabled / metrics disabled 时占位).
func identity(next Endpoint) Endpoint {
	return func(ctx context.Context, req, resp interface{}) error {
		return next(ctx, req, resp)
	}
}
