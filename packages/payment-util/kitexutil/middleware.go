// middleware.go — Kitex 服务端 / 客户端共用 middleware. 真接 Kitex endpoint.Endpoint
// + metainfo + Prometheus, 跟现有 grpcsvc / obsbootstrap interceptor 等价.
//
// 跟老 grpc UnaryInterceptor 对应关系:
//
//	grpc.ChainUnaryInterceptor(...)  → server.WithMiddleware(kitexutil.Chain(...))
//	grpc.WithChainUnaryInterceptor() → client.WithMiddleware(kitexutil.Chain(...))
//
// 业务代码 server/client 构造时:
//
//	srv := <svc>service.NewServer(impl,
//	    server.WithMiddleware(kitexutil.AuthMW(token)),
//	    server.WithMiddleware(kitexutil.LogMW(log)),
//	    server.WithMiddleware(kitexutil.MetricsMW()),
//	    server.WithMiddleware(kitexutil.RecoverMW(log)),
//	)
//	cli, _ := <svc>service.NewClient("dest",
//	    client.WithHostPorts(addr),
//	    client.WithMiddleware(kitexutil.ShadowMW()),
//	)

package kitexutil

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// Endpoint Kitex Unary endpoint (业务 handler 的统一签名).
// 别名导出, 业务代码不需要直接 import kitex/pkg/endpoint.
type Endpoint = endpoint.Endpoint

// Middleware Endpoint 装饰器链 — 跟 Kitex endpoint.Middleware 同名同款.
type Middleware = endpoint.Middleware

// Chain 把多个 Middleware 按调用顺序串成单条链 (跟 grpc.ChainUnaryInterceptor 等价).
func Chain(mws ...Middleware) Middleware {
	return endpoint.Chain(mws...)
}

// ─── Auth MW ───────────────────────────────────────────────────────────────

const (
	// HeaderAdminToken Kitex TTHeader 里的 admin token 字段名 (lowercase, 跟 gRPC 规范一致).
	HeaderAdminToken = "x-admin-token"
	// HeaderShadow 影子流量标识 — payment-core/channel/risk/kms 看到 shadow=1 不真落账.
	HeaderShadow = "x-shadow"
	// HeaderTraceID 全链路 trace id, 跟 OTel W3C traceparent 平行.
	HeaderTraceID = "x-trace-id"
)

// AuthMW 校验 metainfo "x-admin-token" — 跟 split-payment adminTokenInterceptor 等价.
//
// expectedToken 空时退化为 no-op (DEV 模式, 调用方自己 log warn 提醒).
// 通过 server.WithMiddleware(AuthMW(token)) 装到 Kitex server.
func AuthMW(expectedToken string) Middleware {
	if expectedToken == "" {
		return passthrough
	}
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			tok, _ := metainfo.GetValue(ctx, HeaderAdminToken)
			if tok != expectedToken {
				return fmt.Errorf("kitexutil: invalid or missing %s", HeaderAdminToken)
			}
			return next(ctx, req, resp)
		}
	}
}

// WithAdminToken client side — 把 token 注入 ctx, 自动随 TTHeader 上传到 server.
//
// Kitex metainfo.WithPersistentValue 一次写入, 后续整个 RPC 链路 (含跨服务调用) 自动透传.
func WithAdminToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metainfo.WithPersistentValue(ctx, HeaderAdminToken, token)
}

// WithShadow 把 shadow=1 注入 ctx — 透传到下游服务, 后者短路返 mock 不真发外部渠道.
func WithShadow(ctx context.Context) context.Context {
	return metainfo.WithPersistentValue(ctx, HeaderShadow, "1")
}

// IsShadow 判断当前 ctx 是不是 shadow 流量 (server side handler 内调用).
func IsShadow(ctx context.Context) bool {
	v, ok := metainfo.GetValue(ctx, HeaderShadow)
	return ok && v == "1"
}

// ShadowMW server side — 把 metainfo x-shadow 翻进 ctx 留给 handler IsShadow 决策.
//
// Kitex 的 metainfo 已经把 TTHeader 自动放进 ctx, 所以 ShadowMW 实际不用做转换;
// 留这个 MW 是为了 metric 记录 + 跟老 shadow.UnaryServerInterceptor 形态对齐.
func ShadowMW() Middleware {
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			if IsShadow(ctx) {
				shadowRPCCount.Inc()
			}
			return next(ctx, req, resp)
		}
	}
}

// ─── Log MW ────────────────────────────────────────────────────────────────

// LogMW access log — RPC 入口/出口 + duration + error.
//
// 跟 grpcsvc.AccessLogInterceptor 等价, 字段: method / caller / dur_ms / err.
// 从 rpcinfo.GetRPCInfo(ctx) 拿 method / from-service / to-service.
func LogMW(log *zap.Logger) Middleware {
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			start := time.Now()
			err := next(ctx, req, resp)
			elapsed := time.Since(start)

			ri := rpcinfo.GetRPCInfo(ctx)
			fields := []zap.Field{zap.Duration("dur", elapsed)}
			if ri != nil {
				if m := ri.To(); m != nil {
					fields = append(fields,
						zap.String("svc", m.ServiceName()),
						zap.String("method", m.Method()))
				}
				if f := ri.From(); f != nil {
					fields = append(fields, zap.String("caller", f.ServiceName()))
				}
			}
			if err != nil {
				log.Warn("kitex rpc error", append(fields, zap.Error(err))...)
			} else {
				log.Debug("kitex rpc ok", fields...)
			}
			return err
		}
	}
}

// ─── Metrics MW ────────────────────────────────────────────────────────────

var (
	rpcDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kitex_rpc_duration_seconds",
		Help:    "Kitex RPC duration histogram (跟 grpcsvc.MetricsInterceptor 同口径)",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"service", "method", "status"})

	rpcCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kitex_rpc_total",
		Help: "Kitex RPC counter (status=ok / err)",
	}, []string{"service", "method", "status"})

	shadowRPCCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kitex_shadow_rpc_total",
		Help: "Kitex shadow=1 RPC counter",
	})
)

// MetricsMW per-RPC 计数 + p50/p95 直方图. 跟 grpcsvc.MetricsInterceptor 形态对齐.
func MetricsMW() Middleware {
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			start := time.Now()
			err := next(ctx, req, resp)
			elapsed := time.Since(start).Seconds()

			var svc, method string
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if m := ri.To(); m != nil {
					svc = m.ServiceName()
					method = m.Method()
				}
			}
			status := "ok"
			if err != nil {
				status = "err"
			}
			rpcDuration.WithLabelValues(svc, method, status).Observe(elapsed)
			rpcCount.WithLabelValues(svc, method, status).Inc()
			return err
		}
	}
}

// ─── Recover MW ────────────────────────────────────────────────────────────

// RecoverMW panic recover → error.
//
// 跟 grpcsvc.PanicRecoverInterceptor 等价 — handler panic 时不挂进程, 转 error
// 返回上游, 同时 zap.Error + 栈.
func RecoverMW(log *zap.Logger) Middleware {
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			defer func() {
				if r := recover(); r != nil {
					var svc, method string
					if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
						if m := ri.To(); m != nil {
							svc = m.ServiceName()
							method = m.Method()
						}
					}
					log.Error("kitex rpc panic",
						zap.String("svc", svc),
						zap.String("method", method),
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

// CircuitBreakerConfig per-RPC 熔断参数. 跟 observability.CircuitBreaker 等价.
type CircuitBreakerConfig struct {
	FailureThreshold int           // 连续失败 N 次 → open. 默认 5.
	SuccessThreshold int           // half-open 后连续成功 N 次 → close. 默认 2.
	OpenDuration     time.Duration // open 状态持续时间, 之后转 half-open. 默认 30s.
}

// CircuitBreakerMW per-target 熔断. cfg 字段 0 走默认值.
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
		state         int32 // 0=closed, 1=open, 2=half-open
		consecutiveOK int32
		consecutiveKO int32
		openedAt      int64 // unix nanos
	)
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			st := atomic.LoadInt32(&state)
			now := time.Now().UnixNano()
			if st == 1 {
				if time.Duration(now-atomic.LoadInt64(&openedAt)) < cfg.OpenDuration {
					return errors.New("kitexutil: circuit breaker OPEN")
				}
				atomic.CompareAndSwapInt32(&state, 1, 2) // → half-open 试探
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

// ─── RateLimit MW ──────────────────────────────────────────────────────────

// RateLimitMW 简单 token bucket 限流, server side 用.
//
// rps <= 0 → no-op. 跟 grpcsvc.RateLimitInterceptor 等价 (但更简单, 没接 per-merchant
// resolver — kms-manage / payment-channel 各自的 per-merchant 限流后续单独 port).
func RateLimitMW(rps float64, burst int) Middleware {
	if rps <= 0 {
		return passthrough
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	var (
		mu        sync.Mutex
		tokens    = float64(burst)
		lastFill  = time.Now()
		burstCap  = float64(burst)
		rateLimit = rps
	)
	return func(next Endpoint) Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			mu.Lock()
			now := time.Now()
			elapsed := now.Sub(lastFill).Seconds()
			tokens = tokens + elapsed*rateLimit
			if tokens > burstCap {
				tokens = burstCap
			}
			lastFill = now
			if tokens < 1 {
				mu.Unlock()
				return errors.New("kitexutil: rate limit exceeded")
			}
			tokens--
			mu.Unlock()
			return next(ctx, req, resp)
		}
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// passthrough no-op middleware (Auth / RateLimit 等条件 disabled 时占位).
func passthrough(next Endpoint) Endpoint {
	return func(ctx context.Context, req, resp interface{}) error {
		return next(ctx, req, resp)
	}
}

// TraceIDFromCtx 从 ctx 取 trace id (TTHeader 透传过来的). 找不到返空.
func TraceIDFromCtx(ctx context.Context) string {
	v, _ := metainfo.GetValue(ctx, HeaderTraceID)
	return v
}

// WithTraceID client side — 把 trace id 注入 ctx, 自动随 TTHeader 上传.
// gen 留空时自动生成一个 (timestamp + 6 hex).
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		id = fmt.Sprintf("%d-%06x", time.Now().UnixNano(), time.Now().Nanosecond()&0xffffff)
	}
	id = strings.TrimSpace(id)
	return metainfo.WithPersistentValue(ctx, HeaderTraceID, id)
}
