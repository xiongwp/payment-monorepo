// Package server: HTTP 入口骨架。
//
// 设计：
//   - 公网 HTTP（:8080）跑业务路由：/v1/*；middleware 链：recovery → logging
//     → rate-limit → auth → handler。
//   - 内部管理 HTTP（:8081）跑 health probe + 路由热重载（待实现）。
//   - Prometheus metrics 在独立端口（:9090），由 cmd/server/main.go 起。
//
// 当前是骨架：除 /health + /v1/ping 外没有具体业务路由。后续 PR 接 order-core /
// payment-core / user-merchant-core 等下游 gRPC stub 做 HTTP→gRPC 透传。
package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/xiongwp/payment-util/healthx"
	"go.uber.org/zap"
)

// Config HTTP server 配置（从 viper 解析，cmd/server 装填）。
type Config struct {
	HTTPPort          int
	AdminPort         int
	MaxRequestBytes   int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// Auth
	AuthEnabled bool
	AuthTokens  []string

	// Rate limit
	IPRPS         int
	IPBurst       int
	MerchantRPS   int
	MerchantBurst int

	// Admin token；空 = warn-only。
	AdminToken string

	// Shadow flag 边界控制（防 X-Shadow header 外部伪造）。
	// 默认（全空）= 所有外部 X-Shadow header 都被 strip 掉，最安全。
	// 内网压测平台 / 内部服务调用要透传 shadow flag，必须配 TrustedCIDRs 或 TrustedHeader*。
	ShadowTrustedCIDRs       []string
	ShadowTrustedHeaderName  string
	ShadowTrustedHeaderValue string
}

// Server 持有公网 HTTP + 内部 admin HTTP 两个 *http.Server，统一启停。
type Server struct {
	cfg    Config
	public *http.Server
	admin  *http.Server
	logger *zap.Logger
}

// draining：SIGTERM 后置 true，让 /readyz 立即 503，K8s 摘流量然后 graceful
// shutdown。
var draining atomic.Bool

// BeginDrain 进入 drain 模式（main.go OnStop 调）。
func BeginDrain() { draining.Store(true) }

// MuxRegister 让外部模块（userweb / 其它）把路由挂到 public mux 上。
// 在 auth / rate-limit / logging / recovery 中间件链 *内* 看到这些路由。
type MuxRegister func(*http.ServeMux)

// NewServer 构造 Server。中间件链在此组合，不在外部 mux 处理；保证所有路由
// 享受同一鉴权 / 限流 / 日志层。registers 让 main.go 装配额外路由（如
// userweb signup/login）。
func NewServer(cfg Config, logger *zap.Logger, registers ...MuxRegister) *Server {
	publicMux := http.NewServeMux()
	for _, reg := range registers {
		if reg != nil {
			reg(publicMux)
		}
	}
	// 公网 /health 沿用 cheap liveness（K8s 不带 X-API-Key 不能通过 auth 中间件）。
	publicMux.HandleFunc("/health", healthx.Liveness)
	publicMux.HandleFunc("/healthz", healthx.Liveness)
	// /readyz 包含 drain 检查；具体 downstream gRPC probe 由 main 注入。
	publicMux.HandleFunc("/readyz", healthx.Readiness(
		healthx.ProbeFunc{N: "drain", F: func(_ context.Context) error {
			if draining.Load() {
				return errDraining
			}
			return nil
		}},
	))
	publicMux.HandleFunc("/v1/ping", handlePing)

	// 中间件按外→内顺序 wrap。最外层 recovery 兜底任何 panic。
	// shadow 在鉴权之后、限流之前：要 APIKey 验过的可信调用方才信任 X-Shadow header；
	// shadow 流量进 ctx 后限流 / 日志可以单独打 label（如有需要）。
	var publicHandler http.Handler = publicMux
	publicHandler = ShadowMiddleware(ShadowConfig{
		TrustedCIDRs:       cfg.ShadowTrustedCIDRs,
		TrustedHeaderName:  cfg.ShadowTrustedHeaderName,
		TrustedHeaderValue: cfg.ShadowTrustedHeaderValue,
	}, logger)(publicHandler)
	if cfg.AuthEnabled {
		publicHandler = APIKeyMiddleware(cfg.AuthTokens, logger)(publicHandler)
	}
	publicHandler = RateLimitMiddleware(cfg.IPRPS, cfg.IPBurst, cfg.MerchantRPS, cfg.MerchantBurst, logger)(publicHandler)
	publicHandler = LoggingMiddleware(logger)(publicHandler)
	publicHandler = RecoverMiddleware(logger)(publicHandler)

	if cfg.MaxRequestBytes > 0 {
		publicHandler = http.MaxBytesHandler(publicHandler, cfg.MaxRequestBytes)
	}

	publicSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:           publicHandler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// Admin server: health 探针 + （未来）路由热重载。token 校验单独包一层。
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/admin/health", handleHealth)
	adminMux.HandleFunc("/admin/health/readiness", handleHealth)

	adminHandler := adminTokenMiddleware(cfg.AdminToken, logger)(adminMux)
	adminHandler = RecoverMiddleware(logger)(adminHandler)

	adminSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.AdminPort),
		Handler:           adminHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       5 * time.Second,
	}

	return &Server{cfg: cfg, public: publicSrv, admin: adminSrv, logger: logger}
}

// Start 异步启动两个 HTTP server。返回的 error chan 收到第一个错误即代表服务异常。
func (s *Server) Start() error {
	s.logger.Info("api-gateway public listening", zap.String("addr", s.public.Addr))
	s.logger.Info("api-gateway admin listening", zap.String("addr", s.admin.Addr))
	go func() {
		if err := s.public.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("public http server error", zap.Error(err))
		}
	}()
	go func() {
		if err := s.admin.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("admin http server error", zap.Error(err))
		}
	}()
	return nil
}

// Stop 优雅关停两个 server。
func (s *Server) Stop(ctx context.Context) error {
	pubErr := s.public.Shutdown(ctx)
	admErr := s.admin.Shutdown(ctx)
	if pubErr != nil {
		return pubErr
	}
	return admErr
}

// errDraining 让 /readyz JSON body 显示明确 reason。
type errDrainingT struct{}

func (errDrainingT) Error() string { return "draining" }

var errDraining = errDrainingT{}

// ─── handlers ────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func handlePing(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"pong":true}`))
}

// adminTokenMiddleware 校验 X-Admin-Token。空 token = warn-only。
// 健康探针豁免（K8s probe 不带 header）。
func adminTokenMiddleware(token string, logger *zap.Logger) func(http.Handler) http.Handler {
	if token == "" {
		logger.Error("admin http: AUTH DISABLED — set ADMIN_HTTP_TOKEN env or admin.token in config for production")
		return func(next http.Handler) http.Handler { return next }
	}
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/admin/health", "/admin/health/readiness":
				next.ServeHTTP(w, r)
				return
			}
			got := []byte(r.Header.Get("X-Admin-Token"))
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized: missing or invalid X-Admin-Token"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
