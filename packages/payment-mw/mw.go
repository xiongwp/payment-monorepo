// Package mw — payment 业务服务通用 HTTP middleware。
//
// 8 个新服务 (billing / gateway / dispute / refund / kyc / audit / webhook /
// biz-admin) 都需要这些横切关注点：
//
//   1. API-Key 鉴权 (商户/ops 区分)
//   2. Prometheus 请求指标 (count + duration + status code)
//   3. Trace ID 透传 (X-Trace-ID header 自动 propagate)
//   4. 请求日志 (zap structured)
//   5. Panic recovery
//   6. CORS (给 biz-admin-web 前端用)
//
// 使用方式：
//
//   chain := mw.Chain(
//       mw.Recover(logger),
//       mw.RequestLog(logger),
//       mw.Trace(),
//       mw.Metrics("billing-system"),
//       mw.Auth(authConfig),
//   )
//   srv := &http.Server{Handler: chain(mux)}

package mw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// ─── chain helper ───────────────────────────────────────────────────

type Middleware func(http.Handler) http.Handler

func Chain(mws ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

// ─── trace_id 透传 ──────────────────────────────────────────────────

type traceKey struct{}

// TraceFromCtx 取出 ctx 里的 trace_id（业务代码用）。
func TraceFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey{}).(string); ok {
		return v
	}
	return ""
}

// Trace 中间件 — 若 header 有 X-Trace-ID 沿用，否则自动生成 + 写 response header。
func Trace() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tid := r.Header.Get("X-Trace-ID")
			if tid == "" {
				tid = newTraceID()
			}
			w.Header().Set("X-Trace-ID", tid)
			ctx := context.WithValue(r.Context(), traceKey{}, tid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newTraceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ─── 请求日志 ───────────────────────────────────────────────────────

// statusRecorder 拦截 WriteHeader 拿 status code。
type statusRecorder struct {
	http.ResponseWriter
	status int
	size   int
}

func (sr *statusRecorder) WriteHeader(code int) { sr.status = code; sr.ResponseWriter.WriteHeader(code) }
func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = 200
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.size += n
	return n, err
}

// RequestLog 结构化日志每个请求。
func RequestLog(log *zap.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(sr, r)
			log.Info("http",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", sr.status),
				zap.Int("size", sr.size),
				zap.Duration("dur", time.Since(start)),
				zap.String("trace_id", TraceFromCtx(r.Context())),
				zap.String("remote", r.RemoteAddr),
			)
		})
	}
}

// ─── Panic recovery ─────────────────────────────────────────────────

// Recover 捕 panic 写 500 + log + 不让进程挂。
func Recover(log *zap.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered",
						zap.Any("panic", rec),
						zap.String("path", r.URL.Path),
						zap.ByteString("stack", debug.Stack()),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ─── Prometheus metrics ─────────────────────────────────────────────

var (
	httpRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "biz_http_requests_total",
			Help: "Total HTTP requests",
		},
		[]string{"service", "method", "path", "status"},
	)
	httpDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "biz_http_request_duration_seconds",
			Help:    "HTTP request duration",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10},
		},
		[]string{"service", "method", "path"},
	)
)

// Metrics 中间件 — 注: path 用 mux pattern 而不是原始（避免 /diffs/12345 这种爆炸）。
//
// 这里简化用 r.URL.Path 的前 2 段 (/api/v1)，更精细用 chi.RouteContext 等。
func Metrics(serviceName string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(sr, r)
			path := normalizePath(r.URL.Path)
			httpRequests.WithLabelValues(serviceName, r.Method, path,
				fmt.Sprintf("%d", sr.status)).Inc()
			httpDuration.WithLabelValues(serviceName, r.Method, path).
				Observe(time.Since(start).Seconds())
		})
	}
}

// normalizePath 简化 path — 取前 3 段防 cardinality 爆炸。
//   /api/v1/diffs/12345     → /api/v1/diffs
//   /api/v1/refunds/9/approve → /api/v1/refunds
func normalizePath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) <= 3 {
		return p
	}
	return "/" + strings.Join(parts[:3], "/")
}

// ─── API-Key 鉴权 ───────────────────────────────────────────────────

// AuthConfig 鉴权配置。
type AuthConfig struct {
	// 公开端点（不鉴权）— 默认 /healthz / /metrics。
	PublicPaths []string

	// 商户 API key (从 header X-API-Key) — 真实生产从 user-merchant-core 拉
	MerchantKeys map[string]string // key → merchant_id

	// ops API key
	OpsKeys map[string]string // key → ops_email

	// OAuth2 Bearer JWT 验签器（可选）— 配了就支持 Authorization: Bearer <jwt>
	// 优先级: Bearer > X-API-Key > X-Internal-Token
	BearerJWT *JWTVerifier
}

type actorKey struct{}

// Actor 鉴权后的当事人信息。
type Actor struct {
	Type       string   // "merchant" / "ops" / "internal" / "service" / "anonymous"
	MerchantID string   // type=merchant 时
	OpsEmail   string   // type=ops 时
	ClientID   string   // OAuth2 client_id (Bearer 流来源)
	ServiceID  string   // type=service 时 (owner_id of OAuth client)
	Scopes     []string // OAuth2 scope list
}

// ActorFromCtx 业务代码取当事人。
func ActorFromCtx(ctx context.Context) Actor {
	if v, ok := ctx.Value(actorKey{}).(Actor); ok {
		return v
	}
	return Actor{Type: "anonymous"}
}

// Auth 鉴权 middleware。
//
// 规则 (按优先级):
//   1. public path → 允许 anonymous
//   2. Authorization: Bearer <jwt> + BearerJWT 配置 → OAuth2 验签
//      claims.owner_type=merchant → actor=merchant
//      claims.owner_type=service  → actor=service
//      claims.owner_type=ops      → actor=ops
//   3. X-API-Key 在 MerchantKeys → actor=merchant
//   4. X-API-Key 在 OpsKeys → actor=ops
//   5. X-Internal-Token = INTERNAL_TOKEN env → actor=internal (服务间调用，遗留)
//   都没匹配 → 401
func Auth(cfg AuthConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// public path 检查
			for _, p := range cfg.PublicPaths {
				if r.URL.Path == p {
					ctx := context.WithValue(r.Context(), actorKey{}, Actor{Type: "anonymous"})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			var actor Actor

			// 1) Bearer JWT (OAuth2)
			if cfg.BearerJWT != nil {
				if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
					tok := strings.TrimPrefix(h, "Bearer ")
					if claims, err := cfg.BearerJWT.Verify(tok); err == nil {
						actor = actorFromClaims(claims)
					} else {
						http.Error(w, "invalid bearer token: "+err.Error(), http.StatusUnauthorized)
						return
					}
				}
			}

			// 2) 遗留 API-Key / Internal-Token
			if actor.Type == "" {
				apikey := r.Header.Get("X-API-Key")
				internalTok := r.Header.Get("X-Internal-Token")
				if internalTok != "" && internalTok == cfg.internalToken() {
					actor = Actor{Type: "internal"}
				} else if apikey != "" {
					if mid, ok := cfg.MerchantKeys[apikey]; ok {
						actor = Actor{Type: "merchant", MerchantID: mid}
					} else if email, ok := cfg.OpsKeys[apikey]; ok {
						actor = Actor{Type: "ops", OpsEmail: email}
					}
				}
			}
			if actor.Type == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), actorKey{}, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// actorFromClaims 把 JWT claims 映射到 Actor。
func actorFromClaims(claims map[string]any) Actor {
	cid, _ := claims["client_id"].(string)
	ownerType, _ := claims["owner_type"].(string)
	ownerID, _ := claims["owner_id"].(string)
	scopeStr, _ := claims["scope"].(string)
	scopes := []string{}
	for _, s := range strings.Fields(strings.ReplaceAll(scopeStr, ",", " ")) {
		if s != "" {
			scopes = append(scopes, s)
		}
	}
	a := Actor{ClientID: cid, Scopes: scopes}
	switch ownerType {
	case "merchant":
		a.Type = "merchant"
		a.MerchantID = ownerID
	case "service":
		a.Type = "service"
		a.ServiceID = ownerID
	case "ops":
		a.Type = "ops"
		a.OpsEmail = ownerID
	default:
		a.Type = ownerType
	}
	return a
}

func (c AuthConfig) internalToken() string {
	// 从环境变量拉 — 服务间调用用统一 token (生产换 mTLS)
	return getEnv("INTERNAL_TOKEN", "dev-internal-token-CHANGE-IN-PROD")
}

// ─── CORS ───────────────────────────────────────────────────────────

// CORS 简化 — 允许 biz-admin-web 同源调，其它 origin 也通过（生产收紧）。
func CORS(allowOrigins []string) Middleware {
	allow := strings.Join(allowOrigins, ",")
	if allow == "" {
		allow = "*"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", allow)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, X-Trace-ID, X-Internal-Token, X-Reviewer, X-Admin-User")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ─── env helper ─────────────────────────────────────────────────────

func getEnv(k, d string) string {
	if v := osGetenv(k); v != "" {
		return v
	}
	return d
}

// osGetenv 抽出来方便 test 注入 — 这里直接调系统 os.Getenv。
func osGetenv(k string) string {
	// 用反射避免 mw 包硬依赖 os；但实际生产用 os.Getenv 就好。
	// 简化版直接 import：
	return _osGetenv(k)
}
